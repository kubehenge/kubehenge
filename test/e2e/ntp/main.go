// Command ntp answers NTP queries from the system clock, for end-to-end lanes whose guests reach no
// public time server, and asks such a server whether its answer is one a client would take.
//
//	ntp serve <address>   answers every query on the UDP address until stopped
//	ntp ask <address>     queries the address once and fails unless the answer is usable
package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"time"
)

const (
	packetSize = 48
	modeClient = 3
	modeServer = 4
	modeMask   = 0x07
	// The version bits of the first byte, which an answer copies from its query.
	versionMask = 0x38
	// Leap indicator 0, version 4, client mode.
	clientQuery = 0x23
	// A primary server, as the system clock is the reference.
	stratum = 1
	// Stratum 16 and above mean unsynchronised.
	maxStratum = 16
	// The clock's precision, 2^-20 s, as the signed exponent NTP sends.
	precision = 0xec
	// About a millisecond, in NTP's 16.16 fixed point.
	rootDispersion = 0x42
	// Leap indicator 3: the server's clock is not synchronised.
	notInSync = 3
	// What beevik/ntp, which Talos uses, allows for half the root delay plus the root dispersion.
	maxRootDistance = 16 * time.Second
	// How far the answer may lie from this machine's clock, which serves it on the runner.
	maxOffset   = 5 * time.Second
	askTimeout  = 2 * time.Second
	receiveSize = 512
	// The fraction bits of NTP's 32.32 timestamps and of its 16.16 short format.
	fractionBits      = 32
	shortFractionBits = 16
	fractionMask      = 1<<fractionBits - 1
	// The program's name, a command and an address.
	argCount  = 3
	exitUsage = 2
)

// ntpEpoch is where NTP's timestamps count from.
var ntpEpoch = time.Date(1900, time.January, 1, 0, 0, 0, 0, time.UTC)

func main() {
	if len(os.Args) != argCount {
		fmt.Fprintln(os.Stderr, "usage: ntp serve <address> | ntp ask <address>")
		os.Exit(exitUsage)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = listenAndServe(os.Args[2])
	case "ask":
		err = ask(os.Args[2], time.Now)
	default:
		fmt.Fprintln(os.Stderr, "usage: ntp serve <address> | ntp ask <address>")
		os.Exit(exitUsage)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ntp:", err)
		os.Exit(1)
	}
}

func listenAndServe(address string) error {
	var config net.ListenConfig
	conn, err := config.ListenPacket(context.Background(), "udp", address)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", address, err)
	}
	defer func() { _ = conn.Close() }()
	fmt.Fprintf(os.Stderr, "answering NTP queries on %s\n", conn.LocalAddr())
	return serve(conn, time.Now)
}

// serve answers each client query on the connection until it is closed. A query it cannot answer is
// noted and the next one read.
func serve(conn net.PacketConn, now func() time.Time) error {
	query := make([]byte, receiveSize)
	for {
		n, from, err := conn.ReadFrom(query)
		if errors.Is(err, net.ErrClosed) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("reading a query: %w", err)
		}
		reply, ok := answer(query[:n], now(), now)
		if !ok {
			continue
		}
		if _, err := conn.WriteTo(reply, from); err != nil {
			fmt.Fprintf(os.Stderr, "answering %s: %v\n", from, err)
			continue
		}
		fmt.Fprintf(os.Stderr, "answered %s\n", from)
	}
}

// answer is the server's reply to a client's query received at the given time, or false when the
// packet is no client query.
func answer(query []byte, received time.Time, now func() time.Time) ([]byte, bool) {
	if len(query) < packetSize || query[0]&modeMask != modeClient {
		return nil, false
	}
	reply := make([]byte, packetSize)
	reply[0] = query[0]&versionMask | modeServer
	reply[1] = stratum
	reply[2] = query[2]
	reply[3] = precision
	binary.BigEndian.PutUint32(reply[8:], rootDispersion)
	copy(reply[12:16], "LOCL")
	binary.BigEndian.PutUint64(reply[16:], ntpTime(received))
	copy(reply[24:32], query[40:48])
	binary.BigEndian.PutUint64(reply[32:], ntpTime(received))
	binary.BigEndian.PutUint64(reply[40:], ntpTime(now()))
	return reply, true
}

// ask queries the server and fails unless its answer passes the checks a client makes before it
// sets its clock.
func ask(address string, now func() time.Time) error {
	ctx, cancel := context.WithTimeout(context.Background(), askTimeout)
	defer cancel()
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "udp", address)
	if err != nil {
		return fmt.Errorf("reaching %s: %w", address, err)
	}
	defer func() { _ = conn.Close() }()
	deadline, _ := ctx.Deadline()
	if err = conn.SetDeadline(deadline); err != nil {
		return fmt.Errorf("asking %s: %w", address, err)
	}
	query := make([]byte, packetSize)
	query[0] = clientQuery
	sent := ntpTime(now())
	binary.BigEndian.PutUint64(query[40:], sent)
	if _, err = conn.Write(query); err != nil {
		return fmt.Errorf("asking %s: %w", address, err)
	}
	reply := make([]byte, receiveSize)
	n, err := conn.Read(reply)
	if err != nil {
		return fmt.Errorf("%s gave no answer: %w", address, err)
	}
	if err = usable(reply[:n], sent, now()); err != nil {
		return fmt.Errorf("%s answered, but %w", address, err)
	}
	if _, err = fmt.Fprintf(os.Stdout, "%s answers NTP queries\n", address); err != nil {
		return fmt.Errorf("writing the outcome: %w", err)
	}
	return nil
}

// usable checks an answer to the query sent at the given NTP time as beevik/ntp's Validate does,
// and that it lies near this machine's clock.
func usable(reply []byte, sent uint64, now time.Time) error {
	switch {
	case len(reply) < packetSize:
		return fmt.Errorf("the answer is %d bytes, not %d", len(reply), packetSize)
	case reply[0]&modeMask != modeServer:
		return fmt.Errorf("the answer is in mode %d, not %d", reply[0]&modeMask, modeServer)
	case reply[0]>>6 == notInSync:
		return errors.New("its clock is not synchronised")
	case reply[1] == 0 || reply[1] >= maxStratum:
		return fmt.Errorf("its stratum is %d", reply[1])
	case binary.BigEndian.Uint64(reply[24:]) != sent:
		return errors.New("it answers another query")
	}
	reference := goTime(binary.BigEndian.Uint64(reply[16:]))
	transmitted := goTime(binary.BigEndian.Uint64(reply[40:]))
	if transmitted.Before(reference) {
		return errors.New("its time is before its reference time")
	}
	delay := shortDuration(binary.BigEndian.Uint32(reply[4:]))
	dispersion := shortDuration(binary.BigEndian.Uint32(reply[8:]))
	if delay/2+dispersion > maxRootDistance {
		return fmt.Errorf("its root distance is %s", delay/2+dispersion)
	}
	if offset := transmitted.Sub(now).Abs(); offset > maxOffset {
		return fmt.Errorf("its time is %s from this machine's", offset)
	}
	return nil
}

// ntpTime is the time as NTP's 32.32 fixed point seconds since 1900.
func ntpTime(t time.Time) uint64 {
	since := t.Sub(ntpEpoch)
	//nolint:gosec // every time this program reads is after 1900, so the duration is positive.
	return uint64(since/time.Second)<<fractionBits | uint64(since%time.Second)<<fractionBits/uint64(time.Second)
}

// goTime is the time of an NTP 32.32 fixed point timestamp.
func goTime(v uint64) time.Time {
	return ntpEpoch.Add(time.Duration(v>>fractionBits)*time.Second + time.Duration((v&fractionMask)*uint64(time.Second)>>fractionBits))
}

// shortDuration is the duration of NTP's 16.16 fixed point seconds.
func shortDuration(v uint32) time.Duration {
	return time.Duration(uint64(v) * uint64(time.Second) >> shortFractionBits)
}
