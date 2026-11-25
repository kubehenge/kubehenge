package main

import (
	"context"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"
)

// The server's answer passes the checks of the client it answers, through a real socket.
func TestServeAnswersAsAClientAccepts(t *testing.T) {
	t.Parallel()
	var config net.ListenConfig
	conn, err := config.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- serve(conn, time.Now) }()
	if err := ask(conn.LocalAddr().String(), time.Now); err != nil {
		t.Errorf("ask: %v", err)
	}
	conn.Close()
	if err := <-done; err != nil {
		t.Errorf("serve: %v", err)
	}
}

func TestAnswer(t *testing.T) {
	t.Parallel()
	received := time.Date(2026, time.November, 26, 8, 0, 0, 0, time.UTC)
	now := func() time.Time { return received.Add(time.Millisecond) }
	query := make([]byte, packetSize)
	query[0] = clientQuery
	query[2] = 6
	binary.BigEndian.PutUint64(query[40:], 0x0123456789abcdef)
	reply, ok := answer(query, received, now)
	if !ok {
		t.Fatal("a client query got no answer")
	}
	if reply[0] != 0x24 || reply[1] != stratum || reply[2] != 6 {
		t.Errorf("leap, version and mode %#x, stratum %d, poll %d", reply[0], reply[1], reply[2])
	}
	if got := binary.BigEndian.Uint64(reply[24:]); got != 0x0123456789abcdef {
		t.Errorf("originate %#x, not the query's transmit time", got)
	}
	if got := goTime(binary.BigEndian.Uint64(reply[40:])); !got.Equal(now()) && got.Sub(now()).Abs() > time.Microsecond {
		t.Errorf("transmit time %s, not %s", got, now())
	}
	if err := usable(reply, 0x0123456789abcdef, now()); err != nil {
		t.Errorf("its own answer is refused: %v", err)
	}

	for name, packet := range map[string][]byte{
		"short":       query[:packetSize-1],
		"server mode": append([]byte{0x24}, query[1:]...),
	} {
		if _, ok := answer(packet, received, now); ok {
			t.Errorf("%s: answered", name)
		}
	}
}

// Each fault a client refuses an answer for is refused here too.
func TestUsableRefuses(t *testing.T) {
	t.Parallel()
	received := time.Date(2026, time.November, 26, 8, 0, 0, 0, time.UTC)
	query := make([]byte, packetSize)
	query[0] = clientQuery
	binary.BigEndian.PutUint64(query[40:], 7)
	good, _ := answer(query, received, func() time.Time { return received })
	for _, c := range []struct {
		name, want string
		change     func([]byte)
		sent       uint64
		now        time.Time
	}{
		{"not in sync", "not synchronised", func(r []byte) { r[0] |= 0xc0 }, 7, received},
		{"kiss of death", "stratum is 0", func(r []byte) { r[1] = 0 }, 7, received},
		{"unsynchronised stratum", "stratum is 16", func(r []byte) { r[1] = 16 }, 7, received},
		{"client mode", "mode 3", func(r []byte) { r[0] = clientQuery }, 7, received},
		{"another query", "another query", func([]byte) {}, 8, received},
		{"before its reference", "before its reference", func(r []byte) {
			binary.BigEndian.PutUint64(r[16:], ntpTime(received.Add(time.Hour)))
		}, 7, received},
		{"far root", "root distance", func(r []byte) { binary.BigEndian.PutUint32(r[8:], 17<<16) }, 7, received},
		{"far from the clock", "from this machine's", func([]byte) {}, 7, received.Add(time.Minute)},
	} {
		reply := append([]byte{}, good...)
		c.change(reply)
		err := usable(reply, c.sent, c.now)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", c.name, err, c.want)
		}
	}
	if err := usable(good[:packetSize-1], 7, received); err == nil {
		t.Error("a short answer is accepted")
	}
}

// ask fails, within its timeout, against an address where nothing answers.
func TestAskWithoutServer(t *testing.T) {
	t.Parallel()
	var config net.ListenConfig
	conn, err := config.ListenPacket(context.Background(), "udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if err := ask(conn.LocalAddr().String(), time.Now); err == nil {
		t.Error("a silent address answered")
	}
}
