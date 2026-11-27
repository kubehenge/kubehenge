package appliance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
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
	// settleTick is how often the settle time asks again, and how often a report shows it.
	settleTick = time.Second
)

// ServiceCheck tells whether the interface serves after a restart: it answers its health route
// within Wait, on its own certificate, and is still the same process Settle later, systemd having
// restarted it no further meanwhile.
type ServiceCheck struct {
	// CertFile is the certificate the interface serves, which the check trusts and nothing else.
	CertFile string
	// StoredFile is the certificate an administrator gave the interface after the install. The
	// interface serves it in place of CertFile's whenever it reads cleanly, so the check expects it
	// then.
	StoredFile string
	// Address is where the interface listens, host and port.
	Address string
	Wait    time.Duration
	Settle  time.Duration
	// Restarts counts the interface's restarts by systemd. It is the unit's NRestarts when nil.
	Restarts func(ctx context.Context) (int, error)
	// Report, when set, is told each stage as it starts, ticks and ends.
	Report func(HealthEvent)
	// Now and After are the clock the waits run on, the real one when nil.
	Now   func() time.Time
	After func(d time.Duration) <-chan time.Time
}

// HealthStage is a step of the check that a Report shows.
type HealthStage int

// The stages in the order the check runs them.
const (
	// StageConnecting is the wait for the first answer. Left counts down from Total.
	StageConnecting HealthStage = iota
	// StageCertificate says the interface presented File, the certificate expected.
	StageCertificate
	// StageAnswered says the interface answered Elapsed after the check began.
	StageAnswered
	// StageSettling is the wait that the interface stays up. Left counts down from Total.
	StageSettling
	// StageStayedUp ends the check.
	StageStayedUp
)

// HealthEvent is one report of a ServiceCheck.
type HealthEvent struct {
	Stage   HealthStage
	Address string
	File    string
	Total   time.Duration
	Left    time.Duration
	Elapsed time.Duration
}

// CertificateError says the interface presents a certificate other than the one expected.
type CertificateError struct {
	Address, File string
	Expected      *x509.Certificate
	Presented     *x509.Certificate
}

func (e *CertificateError) Error() string {
	return fmt.Sprintf("%s serves a certificate other than %s: expected %s, presented %s",
		e.Address, e.File, describeCertificate(e.Expected), describeCertificate(e.Presented))
}

// describeCertificate names a certificate by its subject, issuer and the start of its fingerprint,
// which tells a replaced certificate from the expected one.
func describeCertificate(c *x509.Certificate) string {
	if c == nil {
		return "none"
	}
	name := c.Subject.CommonName
	switch {
	case name != "":
	case len(c.DNSNames) > 0:
		name = c.DNSNames[0]
	case len(c.IPAddresses) > 0:
		name = c.IPAddresses[0].String()
	default:
		name = "an unnamed certificate"
	}
	issuer := c.Issuer.CommonName
	if issuer == "" {
		issuer = c.Issuer.String()
	}
	sum := sha256.Sum256(c.Raw)
	return fmt.Sprintf("%s (issued by %s, sha256 %s)", name, issuer, hex.EncodeToString(sum[:8]))
}

// Healthy is the check.
func (c *ServiceCheck) Healthy(ctx context.Context) error {
	began := c.now()
	err := c.untilAnswers(ctx, began)
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
	return c.stayUp(ctx, restarts, before)
}

// Client accepts only the served certificate itself, so the check holds for one Proxmox VE's CA signed,
// a self-signed one and an administrator's own alike, and asks for it by a name it is valid for.
// It returns the client and the interface's URL.
func (c *ServiceCheck) Client() (*http.Client, string, error) {
	p, err := c.pinnedClient()
	if err != nil {
		return nil, "", err
	}
	return p.client, p.base, nil
}

// expectedClient is Client's answer with the certificate it expects and the file that came from.
type expectedClient struct {
	client *http.Client
	base   string
	leaf   *x509.Certificate
	file   string
}

func (c *ServiceCheck) pinnedClient() (expectedClient, error) {
	_, port, err := net.SplitHostPort(c.Address)
	if err != nil {
		return expectedClient{}, fmt.Errorf("the interface's address: %w", err)
	}
	leaf, file, err := c.expected()
	if err != nil {
		return expectedClient{}, err
	}
	var name string
	switch {
	case len(leaf.DNSNames) > 0:
		name = leaf.DNSNames[0]
	case len(leaf.IPAddresses) > 0:
		name = leaf.IPAddresses[0].String()
	default:
		return expectedClient{}, errors.New("the interface's certificate names no host")
	}
	// The certificate on disk is the only root, and it is verified as of its first valid moment.
	// Verifying it against the clock fails once it has expired, which would roll back every upgrade
	// from then on, and the interface warns about the expiry itself.
	roots := x509.NewCertPool()
	roots.AddCert(leaf)
	pinned := &tls.Config{
		ServerName: name, MinVersion: tls.VersionTLS12, RootCAs: roots,
		Time: func() time.Time { return leaf.NotBefore },
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 || !bytes.Equal(cs.PeerCertificates[0].Raw, leaf.Raw) {
				var presented *x509.Certificate
				if len(cs.PeerCertificates) > 0 {
					presented = cs.PeerCertificates[0]
				}
				return &CertificateError{Address: c.Address, File: file, Expected: leaf, Presented: presented}
			}
			return nil
		},
	}
	dialer := &net.Dialer{Timeout: healthRequest}
	transport := &http.Transport{
		TLSClientConfig: pinned,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, c.Address)
		},
	}
	return expectedClient{
		client: &http.Client{Transport: transport, Timeout: healthRequest}, base: "https://" + net.JoinHostPort(name, port), leaf: leaf, file: file,
	}, nil
}

func (c *ServiceCheck) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *ServiceCheck) after(d time.Duration) <-chan time.Time {
	if c.After != nil {
		return c.After(d)
	}
	return time.After(d)
}

func (c *ServiceCheck) report(e HealthEvent) {
	if c.Report != nil {
		e.Address = c.Address
		c.Report(e)
	}
}

// stayUp waits Settle, asking each second whether the interface was restarted and still answers,
// so a failure says when it came.
func (c *ServiceCheck) stayUp(ctx context.Context, restarts func(context.Context) (int, error), before int) error {
	start := c.now()
	c.report(HealthEvent{Stage: StageSettling, Total: c.Settle, Left: c.Settle})
	for {
		left := c.Settle - c.now().Sub(start)
		if left <= 0 {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("waiting for the interface to settle: %w", ctx.Err())
		case <-c.after(min(settleTick, left)):
		}
		elapsed := c.now().Sub(start).Round(time.Second)
		after, err := restarts(ctx)
		if err != nil {
			return err
		}
		if after != before {
			return fmt.Errorf("the interface stopped and was restarted %d times, %s into the %s it had to stay up", after-before, elapsed, c.Settle)
		}
		if _, err = c.probe(ctx); err != nil {
			return fmt.Errorf("the interface stopped answering %s into the %s it had to stay up: %w", elapsed, c.Settle, err)
		}
		c.report(HealthEvent{Stage: StageSettling, Total: c.Settle, Left: max(c.Settle-c.now().Sub(start), 0)})
	}
	c.report(HealthEvent{Stage: StageStayedUp, Total: c.Settle})
	return nil
}

// expected is the certificate the interface should serve, and the file it came from.
func (c *ServiceCheck) expected() (*x509.Certificate, string, error) {
	return ServedCertificate(c.CertFile, c.StoredFile, time.Now())
}

// probe asks the health route once, on the certificate on disk now, since ACME may deliver one
// while the check runs. It returns the file that certificate came from.
func (c *ServiceCheck) probe(ctx context.Context) (string, error) {
	p, err := c.pinnedClient()
	if err != nil {
		return "", err
	}
	err = answers(ctx, p.client, p.base+"/api/health")
	p.client.CloseIdleConnections()
	if verr := (*tls.CertificateVerificationError)(nil); errors.As(err, &verr) {
		var presented *x509.Certificate
		if len(verr.UnverifiedCertificates) > 0 {
			presented = verr.UnverifiedCertificates[0]
		}
		err = &CertificateError{Address: c.Address, File: p.file, Expected: p.leaf, Presented: presented}
	}
	return p.file, err
}

// untilAnswers asks until the interface answers or Wait has passed since began.
func (c *ServiceCheck) untilAnswers(ctx context.Context, began time.Time) error {
	deadline := began.Add(c.Wait)
	var last error
	for {
		left := max(deadline.Sub(c.now()), 0)
		c.report(HealthEvent{Stage: StageConnecting, Total: c.Wait, Left: left})
		asked, cancel := context.WithTimeout(ctx, left)
		file, err := c.probe(asked)
		cut := asked.Err() != nil
		cancel()
		if err == nil {
			c.report(HealthEvent{Stage: StageCertificate, File: file})
			c.report(HealthEvent{Stage: StageAnswered, Elapsed: c.now().Sub(began)})
			return nil
		}
		// A request the deadline cut off says less than the answer before it.
		if last == nil || !cut {
			last = err
		}
		left = deadline.Sub(c.now())
		if left <= 0 {
			return fmt.Errorf("the interface at %s did not answer within %s: %w", c.Address, c.Wait, last)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("the interface at %s did not answer within %s: %w", c.Address, c.Wait, last)
		case <-c.after(min(healthPoll, left)):
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
