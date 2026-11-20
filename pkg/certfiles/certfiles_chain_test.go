package certfiles

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestMaterialSplitOverFiles(t *testing.T) {
	t.Parallel()
	p := newPKI(t, "p256")
	items := [][]byte{keyPKCS8PEM(t, p.leafKey), certPEM(p.leaf), certPEM(p.inter), certPEM(p.root)}
	t.Run("one file in any order", func(t *testing.T) {
		t.Parallel()
		reversed := slices.Clone(items)
		slices.Reverse(reversed)
		wantChain(t, mustRead(t, []File{{"all.pem", join(reversed...)}}, "", nil), p)
	})
	for n := 2; n <= len(items); n++ {
		t.Run(strconv.Itoa(n)+" files", func(t *testing.T) {
			t.Parallel()
			files := make([]File, n)
			for i := range files {
				files[i].Name = "part" + strconv.Itoa(i) + ".pem"
			}
			position := 0
			for _, item := range slices.Backward(items) {
				files[position%n].Data = append(files[position%n].Data, item...)
				position++
			}
			wantChain(t, mustRead(t, files, "", nil), p)
		})
	}
}

func TestOnePrivateKey(t *testing.T) {
	t.Parallel()
	p := newPKI(t, "p256")
	other := newPKI(t, "p256")
	chain := File{"chain.pem", allCerts(p)}
	t.Run("none", func(t *testing.T) {
		t.Parallel()
		_, err := Read([]File{chain, {"more.pem", certPEM(other.leaf)}}, "", nil, testNow)
		if err == nil || err.Error() != "chain.pem, more.pem hold no private key" {
			t.Fatalf("error %v does not say no key was found and name both files", err)
		}
		if _, err = Read([]File{chain}, "", nil, testNow); err == nil || err.Error() != "chain.pem holds no private key" {
			t.Fatalf("error %v does not say one file holds no key", err)
		}
	})
	t.Run("two different", func(t *testing.T) {
		t.Parallel()
		files := []File{chain, {"a.key", keyPKCS8PEM(t, p.leafKey)}, {"b.key", keyPKCS8PEM(t, other.leafKey)}}
		_, err := Read(files, "", nil, testNow)
		if err == nil || !strings.Contains(err.Error(), "a.key") || !strings.Contains(err.Error(), "b.key") ||
			!strings.Contains(err.Error(), "different private keys") {
			t.Fatalf("error %v does not name both key files", err)
		}
	})
	t.Run("same twice", func(t *testing.T) {
		t.Parallel()
		files := []File{chain, {"a.key", keyPKCS8PEM(t, p.leafKey)}, {"b.key", keyPKCS8(t, p.leafKey)}}
		wantChain(t, mustRead(t, files, "", nil), p)
	})
}

func TestKeyBelongsToNoCertificate(t *testing.T) {
	t.Parallel()
	p := newPKI(t, "p256")
	other := newPKI(t, "p256")
	_, err := Read([]File{{"chain.pem", allCerts(p)}, {"other.key", keyPKCS8PEM(t, other.leafKey)}}, "", nil, testNow)
	if err == nil || !strings.Contains(err.Error(), "belongs to none of the certificates") || !strings.Contains(err.Error(), "other.key") {
		t.Fatalf("error %v does not say the key matches no certificate", err)
	}
}

func TestChainFollowsSignatures(t *testing.T) {
	t.Parallel()
	p := newPKI(t, "p256")
	// The impostor has the intermediate's name and a key of its own, so only a signature check
	// tells it from the real issuer.
	impostorKey := genKey(t, "p256")
	impostor := issue(t, "Example Intermediate", x509.KeyUsageCertSign, impostorKey.Public(), nil, impostorKey)
	files := []File{
		{"a.pem", join(certPEM(p.root), certPEM(impostor), certPEM(p.leaf))},
		{"b.pem", join(certPEM(p.inter), certPEM(p.leaf), certPEM(p.root))},
		{"key.pem", keyPKCS8PEM(t, p.leafKey)},
	}
	bundle := mustRead(t, files, "", nil)
	wantChain(t, bundle, p)
	if got := strings.Count(string(bundle.CertPEM), "BEGIN CERTIFICATE"); got != 2 {
		t.Fatalf("CertPEM holds %d certificates, want the leaf and one intermediate without duplicates or the root", got)
	}
	if len(bundle.Dropped) != 1 || bundle.Dropped[0] != impostor.Subject.String() {
		t.Fatalf("Dropped is %q, want only the impostor, since duplicates go silently", bundle.Dropped)
	}
}

func TestDroppedCertificates(t *testing.T) {
	t.Parallel()
	p := newPKI(t, "p256")
	strayKey := genKey(t, "p256")
	stray := issue(t, "stray.example.org", 0, strayKey.Public(), nil, strayKey)
	files := []File{{"all.pem", join(allCerts(p), certPEM(stray), keyPKCS8PEM(t, p.leafKey))}}
	bundle := mustRead(t, files, "", nil)
	wantChain(t, bundle, p)
	if len(bundle.Dropped) != 1 || bundle.Dropped[0] != stray.Subject.String() {
		t.Fatalf("Dropped is %q, want only the stray certificate's subject %q", bundle.Dropped, stray.Subject.String())
	}
}

func TestIncompleteChain(t *testing.T) {
	t.Parallel()
	p := newPKI(t, "p256")
	key := File{"key.pem", keyPKCS8PEM(t, p.leafKey)}
	rootPool := x509.NewCertPool()
	rootPool.AddCert(p.root)
	interPool := x509.NewCertPool()
	interPool.AddCert(p.inter)

	t.Run("self-signed leaf", func(t *testing.T) {
		t.Parallel()
		key := genKey(t, "p256")
		self := issue(t, "self.example.org", 0, key.Public(), nil, key)
		bundle := mustRead(t, []File{{"self.pem", join(certPEM(self), keyPKCS8PEM(t, key))}}, "", nil)
		if bundle.Incomplete || len(bundle.Chain) != 1 || len(bundle.Dropped) != 0 {
			t.Fatalf("Incomplete=%v chain=%d dropped=%q, want a complete chain of the one certificate", bundle.Incomplete, len(bundle.Chain), bundle.Dropped)
		}
	})
	t.Run("issuer missing", func(t *testing.T) {
		t.Parallel()
		for _, roots := range []*x509.CertPool{nil, rootPool} {
			bundle := mustRead(t, []File{key, {"leaf.pem", certPEM(p.leaf)}}, "", roots)
			if !bundle.Incomplete || bundle.MissingIssuer != p.inter.Subject.String() {
				t.Fatalf("Incomplete=%v MissingIssuer=%q, want the intermediate's subject", bundle.Incomplete, bundle.MissingIssuer)
			}
		}
	})
	t.Run("roots hold the issuer", func(t *testing.T) {
		t.Parallel()
		bundle := mustRead(t, []File{key, {"leaf.pem", certPEM(p.leaf)}}, "", interPool)
		if bundle.Incomplete {
			t.Fatalf("Incomplete with the issuer in roots, missing %q", bundle.MissingIssuer)
		}
	})
	t.Run("root not in the files", func(t *testing.T) {
		t.Parallel()
		files := []File{key, {"chain.pem", join(certPEM(p.leaf), certPEM(p.inter))}}
		if bundle := mustRead(t, files, "", nil); !bundle.Incomplete || bundle.MissingIssuer != p.root.Subject.String() {
			t.Fatalf("Incomplete=%v MissingIssuer=%q, want the root's subject", bundle.Incomplete, bundle.MissingIssuer)
		}
		if bundle := mustRead(t, files, "", rootPool); bundle.Incomplete {
			t.Fatal("Incomplete although roots hold the root")
		}
	})
}

func TestLeafValidity(t *testing.T) {
	t.Parallel()
	p := newPKI(t, "p256")
	files := []File{{"chain.pem", allCerts(p)}, {"key.pem", keyPKCS8PEM(t, p.leafKey)}}
	t.Run("expired", func(t *testing.T) {
		t.Parallel()
		_, err := Read(files, "", nil, p.leaf.NotAfter.Add(time.Hour))
		if err == nil || !strings.Contains(err.Error(), "chain.pem") || !strings.Contains(err.Error(), "expired on "+p.leaf.NotAfter.UTC().Format(time.RFC3339)) {
			t.Fatalf("error %v does not name the file and the expiry date", err)
		}
	})
	t.Run("not yet valid", func(t *testing.T) {
		t.Parallel()
		_, err := Read(files, "", nil, p.leaf.NotBefore.Add(-time.Hour))
		if err == nil || !strings.Contains(err.Error(), "not valid before "+p.leaf.NotBefore.UTC().Format(time.RFC3339)) {
			t.Fatalf("error %v does not give the start date", err)
		}
	})
	t.Run("reported", func(t *testing.T) {
		t.Parallel()
		if got := mustRead(t, files, "", nil).Leaf.NotAfter; !got.Equal(p.leaf.NotAfter) {
			t.Fatalf("Leaf.NotAfter is %v, want %v", got, p.leaf.NotAfter)
		}
	})
}

func TestOutputShape(t *testing.T) {
	t.Parallel()
	p := newPKI(t, "ed25519")
	bundle := mustRead(t, []File{{"all.pem", join(keyPKCS8PEM(t, p.leafKey), certPEM(p.root), certPEM(p.inter), certPEM(p.leaf))}}, "", nil)
	rest := bundle.CertPEM
	var got [][]byte
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			t.Fatalf("CertPEM holds a %s block", block.Type)
		}
		got = append(got, block.Bytes)
	}
	if len(got) != 2 || !bytes.Equal(got[0], p.leaf.Raw) || !bytes.Equal(got[1], p.inter.Raw) {
		t.Fatalf("CertPEM holds %d certificates, want the leaf then the intermediate", len(got))
	}
	keyBlock, trailing := pem.Decode(bundle.KeyPEM)
	if keyBlock == nil || keyBlock.Type != "PRIVATE KEY" || len(trailing) != 0 || len(keyBlock.Headers) != 0 {
		t.Fatal("KeyPEM is not one unencrypted PRIVATE KEY block")
	}
	if _, err := x509.ParsePKCS8PrivateKey(keyBlock.Bytes); err != nil {
		t.Fatalf("KeyPEM is not PKCS#8: %v", err)
	}
}
