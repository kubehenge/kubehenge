package appliance

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

const (
	// healthPoll is how often a check asks the restarted interface.
	healthPoll = 2 * time.Second
	// healthRequest bounds one question to it.
	healthRequest = 5 * time.Second
)

// ServiceCheck tells whether the interface serves after a restart: it answers its health route
// within Wait, on its own certificate, and is still the same process Settle later, systemd having
// restarted it no further meanwhile.
type ServiceCheck struct {
	// CertFile is the certificate the interface serves, which the check trusts and nothing else.
	CertFile string
	// Address is where the interface listens, host and port.
	Address string
	Wait    time.Duration
	Settle  time.Duration
	// Restarts counts the interface's restarts by systemd; the unit's NRestarts when nil.
	Restarts func(ctx context.Context) (int, error)
}

// Healthy is the check.
func (c *ServiceCheck) Healthy(ctx context.Context) error {
	client, base, err := c.Client()
	if err != nil {
		return err
	}
	url := base + "/api/health"
	err = c.untilAnswers(ctx, client, url)
	if err != nil {
		return err
	}
	restarts := c.Restarts
	if restarts == nil {
		restarts = serviceRestarts
	}
	before, err := restarts(ctx)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return fmt.Errorf("waiting for the interface to settle: %w", ctx.Err())
	case <-time.After(c.Settle):
	}
	after, err := restarts(ctx)
	if err != nil {
		return err
	}
	if after != before {
		return fmt.Errorf("the interface stopped and was restarted %d times within %s of starting", after-before, c.Settle)
	}
	return answers(ctx, client, url)
}

// Client trusts the served certificate itself, so the check holds for one Proxmox VE's CA signed,
// a self-signed one and an administrator's own alike, and asks for it by a name it is valid for.
// It returns the client and the interface's URL.
func (c *ServiceCheck) Client() (*http.Client, string, error) {
	_, port, err := net.SplitHostPort(c.Address)
	if err != nil {
		return nil, "", fmt.Errorf("the interface's address: %w", err)
	}
	raw, err := os.ReadFile(c.CertFile)
	if err != nil {
		return nil, "", fmt.Errorf("the interface's certificate: %w", err)
	}
	block, _ := pem.Decode(raw)
	if block == nil {
		return nil, "", fmt.Errorf("the interface's certificate %s holds no PEM", c.CertFile)
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, "", fmt.Errorf("the interface's certificate: %w", err)
	}
	var name string
	switch {
	case len(leaf.DNSNames) > 0:
		name = leaf.DNSNames[0]
	case len(leaf.IPAddresses) > 0:
		name = leaf.IPAddresses[0].String()
	default:
		return nil, "", errors.New("the interface's certificate names no host")
	}
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	dialer := &net.Dialer{Timeout: healthRequest}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, ServerName: name, MinVersion: tls.VersionTLS12},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, c.Address)
		},
	}
	return &http.Client{Transport: transport, Timeout: healthRequest}, "https://" + net.JoinHostPort(name, port), nil
}

func (c *ServiceCheck) untilAnswers(ctx context.Context, client *http.Client, url string) error {
	ctx, cancel := context.WithTimeout(ctx, c.Wait)
	defer cancel()
	var last error
	for {
		err := answers(ctx, client, url)
		if err == nil {
			return nil
		}
		// A request the deadline cut off says less than the answer before it.
		if last == nil || ctx.Err() == nil {
			last = err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the interface did not answer within %s: %w", c.Wait, last)
		case <-time.After(healthPoll):
		}
	}
}

func answers(ctx context.Context, client *http.Client, url string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, http.NoBody)
	if err != nil {
		return fmt.Errorf("asking the interface: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("asking the interface: %w", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("the interface answered %s", resp.Status)
	}
	return nil
}

// serviceRestarts is how often systemd restarted the interface since it was last started.
func serviceRestarts(ctx context.Context) (int, error) {
	out, err := exec.CommandContext(ctx, "systemctl", "show", "--property=NRestarts", "--value", "kubehenge.service").Output()
	if err != nil {
		return 0, fmt.Errorf("systemctl show kubehenge.service: %w", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		return 0, fmt.Errorf("systemctl show kubehenge.service: %w", err)
	}
	return n, nil
}
