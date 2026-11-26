package appliance

import (
	"bytes"
	"compress/gzip"
	"crypto/x509"
	_ "embed"
	"encoding/pem"
	"io"
	"slices"
	"sync"
)

// talosRootsGzip is, gzipped, the file of public roots the management cluster's Talos release
// installs as /etc/ssl/certs/ca-certificates: curl's extract of Mozilla's roots, at the date
// siderolabs/tools pins for that release. Go on the nodes trusts every root in it, with no
// distrust dates. The file is kept compressed, since its base64 holds short words by chance.
//
//go:embed talosroots.crt.gz
var talosRootsGzip []byte

// talosRootsRelease is the Talos release whose roots the file holds, and talosRootsSHA256 the
// digest of the file as siderolabs/tools pins it for that release.
const (
	talosRootsRelease = "v1.14.6"
	talosRootsSHA256  = "796f1389bfea13c0ed78f663b765baaff54a508e5483786bd6ece73c5e3e3cbe"
)

// talosRoots is the file as Talos installs it, or none where it cannot be read.
var talosRoots = sync.OnceValue(func() []byte {
	r, err := gzip.NewReader(bytes.NewReader(talosRootsGzip))
	if err != nil {
		return nil
	}
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil
	}
	return raw
})

// nodeRootCerts are the certificates of the file that Go reads. Like the nodes' own loading of
// it, it passes over one Go cannot parse.
var nodeRootCerts = sync.OnceValue(func() []*x509.Certificate {
	var out []*x509.Certificate
	for rest := talosRoots(); ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return out
		}
		if c, err := x509.ParseCertificate(block.Bytes); block.Type == pemCertificate && err == nil {
			out = append(out, c)
		}
	}
})

// nodeRoots are the public roots the nodes trust.
var nodeRoots = sync.OnceValue(func() *x509.CertPool { return poolOf(nodeRootCerts()) })

// publicName says whether name is how Go's verifier names one of the nodes' public roots in an
// error: its common name, else its first organisation, or its whole subject.
func publicName(name string) bool {
	return slices.ContainsFunc(nodeRootCerts(), func(c *x509.Certificate) bool {
		short := c.Subject.CommonName
		if short == "" && len(c.Subject.Organization) > 0 {
			short = c.Subject.Organization[0]
		}
		return name == short || name == c.Subject.String()
	})
}
