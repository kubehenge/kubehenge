package release

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func signedIndex(t *testing.T, versions ...string) ([]byte, string, ed25519.PublicKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	var ix Index
	for _, v := range versions {
		ix.Releases = append(ix.Releases, Release{Version: v, URL: "https://example.org/" + v + ".tar.gz", SHA256: strings.Repeat("a", 64)})
	}
	raw, err := json.Marshal(ix)
	if err != nil {
		t.Fatal(err)
	}
	return raw, Sign(raw, priv), pub
}

func newKey(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	return pub, priv
}

func TestParseVerifiesTheSignature(t *testing.T) {
	t.Parallel()
	raw, sig, pub := signedIndex(t, "0.1.0", "0.2.0")
	if _, err := Parse(raw, sig, pub); err != nil {
		t.Fatal(err)
	}
	tampered := []byte(strings.Replace(string(raw), "0.2.0", "9.9.9", 2))
	if _, err := Parse(tampered, sig, pub); !errors.Is(err, ErrSignature) {
		t.Fatalf("tampered index: %v", err)
	}
	other, _ := newKey(t)
	if _, err := Parse(raw, sig, other); !errors.Is(err, ErrSignature) {
		t.Fatalf("other key: %v", err)
	}
	if _, err := Parse(raw, sig, nil); err == nil {
		t.Fatal("no key accepted an index")
	}
}

func TestAddOnlyGrowsTheIndex(t *testing.T) {
	t.Parallel()
	sum := strings.Repeat("b", 64)
	ix := &Index{}
	for _, v := range []string{"0.1.0", "0.1.1", "0.2.0"} {
		if err := ix.Add(Release{Version: v, URL: "https://example.org/" + v, SHA256: sum}); err != nil {
			t.Fatalf("%s: %v", v, err)
		}
	}
	for _, r := range []Release{
		{Version: "0.2.0", URL: "https://example.org/again", SHA256: sum},
		{Version: "0.1.5", URL: "https://example.org/older", SHA256: sum},
		{Version: "0.3.0", URL: "https://example.org/x", SHA256: strings.Repeat("G", 64)},
		{Version: "0.3.0", URL: "https://example.org/x", SHA256: sum, Tofu: &Artefact{URL: "https://example.org/tofu", SHA256: sum}},
	} {
		if err := ix.Add(r); err == nil {
			t.Errorf("added %+v", r)
		}
	}
	pub, priv := newKey(t)
	raw, err := ix.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(raw, Sign(raw, priv), pub)
	if err != nil || len(back.Releases) != 3 || back.Releases[2].Version != "0.2.0" {
		t.Fatalf("round trip: %+v, %v", back, err)
	}
}

func TestParseRefusesBadEntries(t *testing.T) {
	t.Parallel()
	pub, priv := newKey(t)
	for _, entry := range []string{
		`{"version":"0.2","url":"https://x/a","sha256":"` + strings.Repeat("a", 64) + `"}`,
		`{"version":"0.2.0","url":"http://x/a","sha256":"` + strings.Repeat("a", 64) + `"}`,
		`{"version":"0.2.0","url":"https://x/a","sha256":"abc"}`,
	} {
		raw := []byte(`{"releases":[` + entry + `]}`)
		if _, err := Parse(raw, Sign(raw, priv), pub); err == nil {
			t.Errorf("accepted %s", entry)
		}
	}
}

func TestNewest(t *testing.T) {
	t.Parallel()
	raw, sig, pub := signedIndex(t, "0.1.0", "0.1.4", "0.2.0", "0.3.1", "1.0.0", "0.1.2")
	ix, err := Parse(raw, sig, pub)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		current string
		policy  Policy
		want    string
	}{
		{"0.1.0", PolicyPatch, "0.1.4"},
		{"0.1.0", PolicyMinor, "0.3.1"},
		{"0.1.0", PolicyOff, ""},
		{"0.3.1", PolicyMinor, ""},
		{"1.0.0", PolicyMinor, ""},
		{"dev", PolicyMinor, ""},
	} {
		got := ix.Newest(c.current, c.policy)
		if (got == nil) != (c.want == "") || (got != nil && got.Version != c.want) {
			t.Errorf("Newest(%s, %s) = %+v, want %q", c.current, c.policy, got, c.want)
		}
	}
}

func TestFindNeverGoesBack(t *testing.T) {
	t.Parallel()
	raw, sig, pub := signedIndex(t, "0.1.0", "0.2.0", "1.0.0")
	ix, err := Parse(raw, sig, pub)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := ix.Find("0.2.0", "1.0.0"); err != nil || r.Version != "1.0.0" {
		t.Fatalf("major upgrade: %+v %v", r, err)
	}
	for _, c := range [][2]string{{"0.2.0", "0.1.0"}, {"0.2.0", "0.2.0"}, {"0.2.0", "0.4.0"}, {"dev", "1.0.0"}} {
		if _, err := ix.Find(c[0], c[1]); err == nil {
			t.Errorf("Find(%s, %s) succeeded", c[0], c[1])
		}
	}
}
