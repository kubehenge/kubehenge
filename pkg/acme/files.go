package acme

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"strings"
)

// Files is a request as command-line flags give it. The HMAC key, the DNS credentials and the
// directory's CA come from files, since a process list shows every argument.
type Files struct {
	// Directory is letsencrypt, letsencrypt-staging or an https address.
	Directory          string   `json:"directory"`
	Email              string   `json:"email,omitempty"`
	Proof              string   `json:"proof,omitempty"`
	Profile            string   `json:"profile,omitempty"`
	KeyID              string   `json:"keyID,omitempty"`
	HMACKeyFile        string   `json:"hmacKeyFile,omitempty"`
	DirectoryCAFile    string   `json:"directoryCAFile,omitempty"`
	DNSProvider        string   `json:"dnsProvider,omitempty"`
	DNSCredentialsFile string   `json:"dnsCredentialsFile,omitempty"`
	Resolvers          []string `json:"resolvers,omitempty"`
}

// Request reads the files into the request the interface takes.
func (f *Files) Request() (*Request, error) {
	req := &Request{
		Directory: f.Directory, Email: f.Email, Proof: f.Proof, Profile: f.Profile, DNSProvider: f.DNSProvider, Resolvers: f.Resolvers,
	}
	if f.KeyID != "" || f.HMACKeyFile != "" {
		req.ExternalAccountBinding = &Binding{KeyID: f.KeyID}
		if f.HMACKeyFile != "" {
			raw, err := read(f.HMACKeyFile)
			if err != nil {
				return nil, err
			}
			req.ExternalAccountBinding.HMACKey = strings.TrimRight(string(raw), "\r\n")
		}
	}
	if f.DirectoryCAFile != "" {
		raw, err := read(f.DirectoryCAFile)
		if err != nil {
			return nil, err
		}
		req.DirectoryCA = string(raw)
	}
	if f.DNSCredentialsFile != "" {
		raw, err := read(f.DNSCredentialsFile)
		if err != nil {
			return nil, err
		}
		if req.DNSCredentials, err = ParseCredentials(raw); err != nil {
			return nil, fmt.Errorf("%s: %w", f.DNSCredentialsFile, err)
		}
	}
	return req, nil
}

func read(file string) ([]byte, error) {
	raw, err := os.ReadFile(file) //nolint:gosec // a file the administrator named
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", file, err)
	}
	return raw, nil
}

// ParseCredentials reads NAME=value lines. Blank lines and lines that start with # are skipped.
func ParseCredentials(raw []byte) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, value, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("line %d is not NAME=value. Write one variable per line", n)
		}
		out[strings.TrimSpace(name)] = value
	}
	return out, nil
}
