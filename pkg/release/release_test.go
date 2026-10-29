package release

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

func signedIndex(tb testing.TB, versions ...string) ([]byte, string, []ed25519.PublicKey) {
	tb.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		tb.Fatal(err)
	}
	ix := Index{Expires: testNow.Add(Validity)}
	for _, v := range versions {
		ix.Releases = append(ix.Releases, Release{Version: v, URL: "https://example.org/" + v + ".tar.gz", SHA256: strings.Repeat("a", 64)})
	}
	raw, err := json.Marshal(ix)
	if err != nil {
		tb.Fatal(err)
	}
	return raw, Sign(raw, priv), []ed25519.PublicKey{pub}
}

func newKey(tb testing.TB) (ed25519.PublicKey, ed25519.PrivateKey) {
	tb.Helper()
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		tb.Fatal(err)
	}
	return pub, priv
}

func TestParseVerifiesTheSignature(t *testing.T) {
	t.Parallel()
	raw, sig, keys := signedIndex(t, "0.1.0", "0.2.0")
	if _, err := Parse(raw, sig, keys, testNow); err != nil {
		t.Fatal(err)
	}
	tampered := []byte(strings.Replace(string(raw), "0.2.0", "9.9.9", 2))
	if _, err := Parse(tampered, sig, keys, testNow); !errors.Is(err, ErrSignature) {
		t.Fatalf("tampered index: %v", err)
	}
	other, _ := newKey(t)
	if _, err := Parse(raw, sig, []ed25519.PublicKey{other}, testNow); !errors.Is(err, ErrSignature) {
		t.Fatalf("other key: %v", err)
	}
	if _, err := Parse(raw, sig, nil, testNow); err == nil {
		t.Fatal("no key accepted an index")
	}
	if _, err := Parse(raw, "", keys, testNow); !errors.Is(err, ErrSignature) {
		t.Fatalf("no signature: %v", err)
	}
}

// A rotation has the index signed with the old key and the new one, one line each, and builds
// trust both until the old one goes: either key's line is enough, and a line that does not verify
// is passed over.
func TestParseTakesASignatureOfAnyTrustedKey(t *testing.T) {
	t.Parallel()
	oldPub, oldKey := newKey(t)
	nextPub, nextKey := newKey(t)
	strangerPub, strangerKey := newKey(t)
	ix := Index{Expires: testNow.Add(Validity), Releases: []Release{}}
	raw, err := ix.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	both := Sign(raw, oldKey) + "\n" + Sign(raw, nextKey) + "\n"
	for name, c := range map[string]struct {
		sigs string
		keys []ed25519.PublicKey
		ok   bool
	}{
		"old build, both signatures":   {both, []ed25519.PublicKey{oldPub}, true},
		"new build, both signatures":   {both, []ed25519.PublicKey{nextPub}, true},
		"rotating build, new only":     {Sign(raw, nextKey), []ed25519.PublicKey{oldPub, nextPub}, true},
		"old build, new only":          {Sign(raw, nextKey), []ed25519.PublicKey{oldPub}, false},
		"stranger first, then trusted": {"garbage\n" + Sign(raw, strangerKey) + "\n" + Sign(raw, oldKey), []ed25519.PublicKey{oldPub}, true},
		"stranger only":                {Sign(raw, strangerKey), []ed25519.PublicKey{oldPub, nextPub}, false},
		"stranger trusted by nobody":   {both, []ed25519.PublicKey{strangerPub}, false},
	} {
		if _, err := Parse(raw, c.sigs, c.keys, testNow); (err == nil) != c.ok {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestParseRefusesAnExpiredIndex(t *testing.T) {
	t.Parallel()
	raw, sig, keys := signedIndex(t, "0.1.0")
	if _, err := Parse(raw, sig, keys, testNow.Add(Validity-time.Second)); err != nil {
		t.Fatalf("a second before its expiry: %v", err)
	}
	if _, err := Parse(raw, sig, keys, testNow.Add(Validity)); !errors.Is(err, ErrExpired) {
		t.Fatalf("at its expiry: %v", err)
	}
	// Signing it again takes it whatever its expiry.
	if _, err := Verify(raw, sig, keys); err != nil {
		t.Fatalf("verifying an expired index: %v", err)
	}
	pub, priv := newKey(t)
	unbounded := []byte(`{"releases":[]}`)
	if _, err := Verify(unbounded, Sign(unbounded, priv), []ed25519.PublicKey{pub}); err == nil {
		t.Fatal("took an index without an expiry")
	}
	var ix Index
	ix.Renew(time.Date(2026, 10, 1, 12, 0, 0, 999, time.FixedZone("CET", 3600)))
	if want := time.Date(2026, 10, 15, 11, 0, 0, 0, time.UTC); !ix.Expires.Equal(want) || ix.Expires.Location() != time.UTC {
		t.Fatalf("renewed to %s, want %s", ix.Expires, want)
	}
}

// Every line of release.pub is linked into the builds as a key; one that is not would leave it out
// unseen.
func TestReleasePubHoldsOnlyKeys(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("release.pub")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Fields(string(raw))
	if keys := parseKeys(strings.Join(lines, ",")); len(lines) == 0 || len(keys) != len(lines) {
		t.Fatalf("release.pub has %d lines, %d of them keys", len(lines), len(keys))
	}
}

func TestPublicKeysReadEveryKeyBuiltIn(t *testing.T) {
	t.Parallel()
	a, _ := newKey(t)
	b, _ := newKey(t)
	if keys := parseKeys(base64.StdEncoding.EncodeToString(a) + ", " + base64.StdEncoding.EncodeToString(b) + ",,not-a-key,"); len(keys) != 2 || !keys[0].Equal(a) || !keys[1].Equal(b) {
		t.Fatalf("keys %v", keys)
	}
	if keys := parseKeys(""); len(keys) != 0 {
		t.Fatalf("keys %v from none", keys)
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
	ix.Renew(testNow)
	raw, err := ix.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := Parse(raw, Sign(raw, priv), []ed25519.PublicKey{pub}, testNow)
	if err != nil || len(back.Releases) != 3 || back.Releases[2].Version != "0.2.0" || !back.Expires.Equal(ix.Expires) {
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
		raw := []byte(`{"expires":"2026-10-15T12:00:00Z","releases":[` + entry + `]}`)
		if _, err := Parse(raw, Sign(raw, priv), []ed25519.PublicKey{pub}, testNow); err == nil {
			t.Errorf("accepted %s", entry)
		}
	}
}

func TestNewest(t *testing.T) {
	t.Parallel()
	raw, sig, keys := signedIndex(t, "0.1.0", "0.1.4", "0.2.0", "0.3.1", "1.0.0", "0.1.2")
	ix, err := Parse(raw, sig, keys, testNow)
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
	raw, sig, keys := signedIndex(t, "0.1.0", "0.2.0", "1.0.0")
	ix, err := Parse(raw, sig, keys, testNow)
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

func FuzzParseVersion(f *testing.F) {
	for _, s := range []string{"1.2.3", "v0.10.0", "1.2", "01.2.3", "-1.2.3", "1.2.3-rc.1", "v", "1..3"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		v, err := ParseVersion(s)
		if err != nil {
			return
		}
		if v.String() != strings.TrimPrefix(s, "v") {
			t.Fatalf("ParseVersion(%q) = %s", s, v)
		}
		if again, err := ParseVersion(v.String()); err != nil || again != v {
			t.Fatalf("ParseVersion(%q) = %+v, %v", v.String(), again, err)
		}
	})
}

// The index is signed before it is read, so this reads what a release key could sign: whatever
// Parse accepts holds only valid releases, survives a round trip, and the upgrade choices take it.
func FuzzParse(f *testing.F) {
	pub, priv := newKey(f)
	keys := []ed25519.PublicKey{pub}
	raw, _, _ := signedIndex(f, "0.1.0", "0.2.0")
	f.Add(raw)
	f.Add([]byte(`{"expires":"2026-10-15T12:00:00Z","releases":[{"version":"1.0.0","url":"https://example.org/a","sha256":"` +
		strings.Repeat("0", 64) + `","tofu":{"version":"1.11.0","url":"https://example.org/t","sha256":"` +
		strings.Repeat("f", 64) + `"}}]}`))
	f.Add([]byte(`{"expires":"2026-10-15T12:00:00Z","releases":[{"version":"1.0","url":"http://example.org/a","sha256":"x"}]}`))
	f.Add([]byte(`{"releases":null}`))
	f.Add([]byte(`{"expires":"0001-01-01T00:00:00Z","releases":null}`))
	f.Fuzz(func(t *testing.T, index []byte) {
		ix, err := Verify(index, Sign(index, priv), keys)
		if err != nil {
			return
		}
		for _, r := range ix.Releases {
			if verr := r.validate(); verr != nil {
				t.Fatalf("accepted %+v: %v", r, verr)
			}
		}
		out, err := ix.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		again, err := Verify(out, Sign(out, priv), keys)
		if err != nil || len(again.Releases) != len(ix.Releases) || !again.Expires.Equal(ix.Expires) {
			t.Fatalf("round trip: %+v, %v", again, err)
		}
		for _, current := range []string{"0.0.0", "1.2.3", "dev"} {
			for _, p := range []Policy{PolicyOff, PolicyPatch, PolicyMinor} {
				if r := ix.Newest(current, p); r != nil {
					if _, err := ix.Find(current, r.Version); err != nil {
						t.Fatalf("Newest(%s, %s) offered %s, which Find refuses: %v", current, p, r.Version, err)
					}
				}
			}
		}
	})
}
