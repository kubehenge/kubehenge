package templates

import (
	"bytes"
	"context"
	"crypto"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

// The fixtures are each publisher's real signed checksum files, and pkgs.k8s.io's key.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := fs.ReadFile(os.DirFS("testdata"), name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const (
	almaFixtureDir  = "/almalinux/10/cloud/x86_64_v2/images"
	rockyFixtureDir = "/pub/rocky/10/images/x86_64"
	rockyFixture    = "Rocky-10-GenericCloud-Base-10.2-20260525.0.x86_64.qcow2"
	leapFixtureDir  = "/distribution/leap/16.1/appliances"
	leapFixture     = "Leap-16.1-Immutable.x86_64-Base-qcow-Build3.10.qcow2"
	k8sKeyPath      = "/core:/stable:/v1.37/rpm/repodata/repomd.xml.key"
)

// fakePublishers serves the fixtures in place of every publisher, at the paths they publish them.
// Each file can be swapped for another through the map returned.
func fakePublishers(t *testing.T) (*Sources, map[string][]byte) {
	t.Helper()
	files := map[string][]byte{
		almaFixtureDir + "/CHECKSUM":                           readFixture(t, "almalinux-10/CHECKSUM"),
		almaFixtureDir + "/CHECKSUM.asc":                       readFixture(t, "almalinux-10/CHECKSUM.asc"),
		rockyFixtureDir + "/" + rockyFixture + ".CHECKSUM":     readFixture(t, "rocky-10/"+rockyFixture+".CHECKSUM"),
		rockyFixtureDir + "/" + rockyFixture + ".CHECKSUM.asc": readFixture(t, "rocky-10/"+rockyFixture+".CHECKSUM.asc"),
		leapFixtureDir + "/" + leapFixture + ".sha256":         readFixture(t, "leap-16.1/"+leapFixture+".sha256"),
		leapFixtureDir + "/" + leapFixture + ".sha256.asc":     readFixture(t, "leap-16.1/"+leapFixture+".sha256.asc"),
		k8sKeyPath: readFixture(t, "pkgs.k8s.io-Release.key"),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /schematics", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != TalosSchematic {
			http.Error(w, "unexpected schematic", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"id":"ce4c980550dd2ab1b17bbf2b08801c7eb59418eafe8f279833297925d67c7515"}`)
	})
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		b, ok := files[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(b)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	mirrors := map[string]string{}
	for _, publisher := range []string{"https://repo.almalinux.org", "https://dl.rockylinux.org", "https://download.opensuse.org", "https://pkgs.k8s.io"} {
		mirrors[publisher] = srv.URL
	}
	return &Sources{HTTP: srv.Client(), TalosFactoryURL: srv.URL, Mirrors: mirrors, retryWait: time.Millisecond}, files
}

// The pins are the releases on offer, read with no request: the factory here fails every one.
func TestTalosReleasesAreThePins(t *testing.T) {
	t.Parallel()
	failing := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("no request expected")
	})}
	s := &Sources{HTTP: failing, TalosFactoryURL: "http://factory.invalid", TalosPins: map[string]TalosPin{
		"v1.14.5": {}, "v1.13.4": {}, "v1.14.4": {},
	}}
	got, err := s.TalosReleases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"v1.13.4", "v1.14.4", "v1.14.5"}; !slices.Equal(got, want) {
		t.Errorf("TalosReleases() = %v, want %v", got, want)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// Each family's image is resolved only through its publisher's signature over the checksum file.
func TestResolveEachFamilyVerifiesItsSignedChecksums(t *testing.T) {
	t.Parallel()
	// The builds the fixtures sign, which Renovate's moves of the releases leave as they are.
	leapBuild := Release{Name: "16.1", Build: "Build3.10", Mode: Transactional}
	almaBuild := Release{Name: "10", Build: "10.2-20261012.0", Mode: ImageMode}
	rockyBuild := Release{Name: "10", Build: "10.2-20260525.0", Mode: ImageMode}
	for _, tc := range []struct {
		name    string
		resolve func(*Sources) (Resolved, error)
		sums    string
		want    Resolved
	}{
		{
			"AlmaLinux 10", func(s *Sources) (Resolved, error) { return almaLinux{}.Image(context.Background(), s, almaBuild) },
			almaFixtureDir + "/CHECKSUM",
			Resolved{
				URL:      almaFixtureDir + "/AlmaLinux-10-GenericCloud-10.2-20261012.0.x86_64_v2.qcow2",
				FileName: "template-almalinux-10.2-20261012.0-bf441cb90a56.qcow2",
				Checksum: "bf441cb90a56ee08ec8a3c38d920996e217e79cfdf6e33561afd3408bed5a337", ChecksumAlgorithm: "sha256",
			},
		},
		{
			"Rocky Linux 10", func(s *Sources) (Resolved, error) { return rocky{}.Image(context.Background(), s, rockyBuild) },
			rockyFixtureDir + "/" + rockyFixture + ".CHECKSUM",
			Resolved{
				URL:      rockyFixtureDir + "/" + rockyFixture,
				FileName: "template-rocky-10.2-20260525.0-9fc9e9ff1688.qcow2",
				Checksum: "9fc9e9ff16888bb68ac39b0392e25c9c92684d50c85f1cce6ab549363bbc4b48", ChecksumAlgorithm: "sha256",
			},
		},
		{
			// Its release candidate's files: Kubehenge offers none, but the checks are the same.
			"openSUSE Leap 16.1", func(s *Sources) (Resolved, error) { return leap{}.Image(context.Background(), s, leapBuild) },
			leapFixtureDir + "/" + leapFixture + ".sha256",
			Resolved{
				URL:      leapFixtureDir + "/" + leapFixture,
				FileName: "template-leap-16.1-Build3.10-93c54ee4b49d.qcow2",
				Checksum: "93c54ee4b49dd7fcd7213ebbd59c824077a4ffde5e6be9a8357f79fcd3655b7b", ChecksumAlgorithm: "sha256",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s, files := fakePublishers(t)
			got, err := tc.resolve(s)
			if err != nil {
				t.Fatal(err)
			}
			tc.want.URL = s.TalosFactoryURL + tc.want.URL
			if got != tc.want {
				t.Errorf("Resolve() = %+v, want %+v", got, tc.want)
			}
			// One changed digit of the checksum breaks the signature.
			sum := tc.want.Checksum
			files[tc.sums] = bytes.Replace(files[tc.sums], []byte(sum), []byte("0"+sum[1:]), 1)
			if _, err := tc.resolve(s); err == nil || !strings.Contains(err.Error(), "not signed by") {
				t.Errorf("a tampered checksum file resolved: %v", err)
			}
			// Another publisher's genuine signature is no signature of this one's.
			other := "almalinux-10/CHECKSUM.asc"
			if tc.name == "AlmaLinux 10" {
				other = "rocky-10/" + rockyFixture + ".CHECKSUM.asc"
			}
			files[tc.sums+".asc"] = readFixture(t, other)
			if _, err := tc.resolve(s); err == nil {
				t.Error("another publisher's signature verified")
			}
		})
	}
}

// Resolve goes to the release a node OS names, on its family's publisher.
func TestResolveTheNodeOSesRelease(t *testing.T) {
	t.Parallel()
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	s := &Sources{HTTP: srv.Client(), Mirrors: map[string]string{"https://dl.rockylinux.org": srv.URL}}
	_, r, _ := ReleaseOf(rocky10)
	if _, err := s.Resolve(context.Background(), ImageRef{Kind: NodeOSImage, NodeOS: rocky10}); err == nil {
		t.Fatal("resolved without its checksums")
	}
	want := rockyFixtureDir + "/Rocky-10-GenericCloud-Base-" + r.Build + ".x86_64.qcow2.CHECKSUM"
	if len(asked) != 1 || asked[0] != want {
		t.Errorf("asked for %v, want %s", asked, want)
	}
}

func TestLeapIsNotOfferedBeforeItsRelease(t *testing.T) {
	t.Parallel()
	s, _ := fakePublishers(t)
	if _, err := s.Resolve(context.Background(), ImageRef{Kind: NodeOSImage, NodeOS: NodeOS{Family: leapName, Release: "16.1"}}); err == nil ||
		!strings.Contains(err.Error(), "has not released") {
		t.Errorf("Resolve = %v", err)
	}
	if _, err := (leap{}).Image(context.Background(), s, leap{}.Releases()[0]); err == nil {
		t.Error("an image of no named build resolved")
	}
}

func TestVerifyRefusesWhatIsNoSignatureOfTheKey(t *testing.T) {
	t.Parallel()
	sums := readFixture(t, "almalinux-10/CHECKSUM")
	sig := readFixture(t, "almalinux-10/CHECKSUM.asc")
	alma := signingKey{name: "AlmaLinux OS 10", armoured: almaLinux10Key, fingerprint: AlmaLinux10KeyFingerprint}
	if err := alma.verify(sums, sig); err != nil {
		t.Fatalf("the genuine checksums fail: %v", err)
	}
	for name, k := range map[string]signingKey{
		"another fingerprint pinned": {name: "AlmaLinux OS 10", armoured: almaLinux10Key, fingerprint: Rocky10KeyFingerprint},
		"two keys embedded":          {name: "AlmaLinux OS 10", armoured: slices.Concat(almaLinux10Key, rocky10Key), fingerprint: AlmaLinux10KeyFingerprint},
		"no key embedded":            {name: "AlmaLinux OS 10", fingerprint: AlmaLinux10KeyFingerprint},
	} {
		if err := k.verify(sums, sig); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
	if err := alma.verify(sums, []byte("-----BEGIN PGP SIGNATURE-----\n\n-----END PGP SIGNATURE-----\n")); err == nil {
		t.Error("an empty signature verified")
	}
}

// almaPairPublisher stands in for a CDN that hands out a replaced checksum file beside the old
// signature. staleSigs is how many of the signature's fetches get the old one, which never signs the
// checksum file.
func almaPairPublisher(t *testing.T, staleSigs int32) (*Sources, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var sumsFetches, sigFetches atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case almaFixtureDir + "/CHECKSUM":
			sumsFetches.Add(1)
			_, _ = w.Write(readFixture(t, "almalinux-10/CHECKSUM"))
		case almaFixtureDir + "/CHECKSUM.asc":
			if sigFetches.Add(1) <= staleSigs {
				_, _ = w.Write(readFixture(t, "rocky-10/"+rockyFixture+".CHECKSUM.asc"))
				return
			}
			_, _ = w.Write(readFixture(t, "almalinux-10/CHECKSUM.asc"))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	s := &Sources{HTTP: srv.Client(), Mirrors: map[string]string{"https://repo.almalinux.org": srv.URL}, retryWait: time.Millisecond}
	return s, &sumsFetches, &sigFetches
}

func resolveAlmaForPair(s *Sources) (Resolved, error) {
	return resolveAlmaForPairWith(context.Background(), s)
}

func resolveAlmaForPairWith(ctx context.Context, s *Sources) (Resolved, error) {
	return almaLinux{}.Image(ctx, s, Release{Name: "10", Build: "10.2-20261012.0", Mode: ImageMode})
}

// pairClock is a clock a test moves by the waits it is asked for, so that an hour passes in no time.
type pairClock struct {
	at    time.Time
	waits []time.Duration
}

// use makes s wait on the clock. A build that never stops waiting is cut off after 200 waits, so
// that a test of it fails instead of hanging.
func (c *pairClock) use(s *Sources) {
	c.at = time.Date(2026, 11, 9, 11, 50, 56, 0, time.UTC)
	s.retryWait = 0
	s.now = func() time.Time { return c.at }
	s.sleep = func(ctx context.Context, d time.Duration) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if len(c.waits) >= 200 {
			return errors.New("still waiting after 200 waits")
		}
		c.waits = append(c.waits, d)
		c.at = c.at.Add(d)
		return nil
	}
}

// Fails a build that gives up at the first mismatch, or fetches only the signature again.
func TestAMismatchedPairIsFetchedAgainTogether(t *testing.T) {
	t.Parallel()
	s, sums, sigs := almaPairPublisher(t, 2)
	got, err := resolveAlmaForPair(s)
	if err != nil {
		t.Fatalf("a pair that agrees on the third fetch: %v", err)
	}
	if got.Checksum != "bf441cb90a56ee08ec8a3c38d920996e217e79cfdf6e33561afd3408bed5a337" {
		t.Errorf("checksum %q", got.Checksum)
	}
	if sums.Load() != 3 || sigs.Load() != 3 {
		t.Errorf("%d checksum fetches and %d signature fetches, want 3 of each", sums.Load(), sigs.Load())
	}
}

// Fails a build of the old seven fetches, which gave up after about 10.5 minutes.
func TestAPairThatAgreesAfterTheOldBudgetPasses(t *testing.T) {
	t.Parallel()
	s, sums, _ := almaPairPublisher(t, 9)
	var clock pairClock
	clock.use(s)
	start := time.Date(2026, 11, 9, 11, 50, 56, 0, time.UTC)
	if _, err := resolveAlmaForPair(s); err != nil {
		t.Fatalf("a pair that agrees on the tenth fetch: %v", err)
	}
	if waited := clock.at.Sub(start); waited <= 10*time.Minute+30*time.Second {
		t.Errorf("waited %s, want more than the old 10.5 minutes", waited)
	}
	if sums.Load() != 10 {
		t.Errorf("%d checksum fetches, want 10", sums.Load())
	}
}

// Fails a build that lets the wait keep doubling.
func TestThePairWaitIsCappedAtFiveMinutes(t *testing.T) {
	t.Parallel()
	s, _, _ := almaPairPublisher(t, 8)
	var clock pairClock
	clock.use(s)
	if _, err := resolveAlmaForPair(s); err != nil {
		t.Fatal(err)
	}
	want := []time.Duration{
		10 * time.Second, 20 * time.Second, 40 * time.Second, 80 * time.Second, 160 * time.Second,
		5 * time.Minute, 5 * time.Minute, 5 * time.Minute,
	}
	if !slices.Equal(clock.waits, want) {
		t.Errorf("waits %v, want %v", clock.waits, want)
	}
}

// Fails a build that never gives up, or that gives up before the budget is spent.
func TestAPairThatNeverAgreesFailsOnceTheBudgetIsSpent(t *testing.T) {
	t.Parallel()
	s, sums, sigs := almaPairPublisher(t, 1<<30)
	var clock pairClock
	clock.use(s)
	start := time.Date(2026, 11, 9, 11, 50, 56, 0, time.UTC)
	_, err := resolveAlmaForPair(s)
	if err == nil {
		t.Fatal("a pair that never verifies resolved")
	}
	for _, want := range []string{"/CHECKSUM and ", "/CHECKSUM.asc", "the publisher's checksum and signature disagree", "not signed by"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if waited := clock.at.Sub(start); waited != signedPairBudget {
		t.Errorf("waited %s, want the budget of %s", waited, signedPairBudget)
	}
	if got := len(clock.waits) + 1; int(sums.Load()) != got || int(sigs.Load()) != got {
		t.Errorf("%d checksum fetches and %d signature fetches, want %d of each", sums.Load(), sigs.Load(), got)
	}
	if !Final(err) {
		t.Error("a build that waited the whole budget would be run again")
	}
}

// Fails a build whose wait ignores the context.
func TestACancelledContextEndsThePairWaitAtOnce(t *testing.T) {
	t.Parallel()
	s, _, _ := almaPairPublisher(t, 1<<30)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	s.retryWait = 0
	s.now = func() time.Time {
		// The second reading is the one after the first mismatch.
		calls++
		if calls == 2 {
			cancel()
		}
		return time.Date(2026, 11, 9, 11, 50, 56, 0, time.UTC)
	}
	started := time.Now()
	_, err := resolveAlmaForPairWith(ctx, s)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v, want the context's", err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Errorf("took %s, want the wait to end at once", took)
	}
}

// A wrong embedded key is no cache's doing, so fetching again would only delay the error.
func TestAWrongEmbeddedKeyIsNotFetchedAgain(t *testing.T) {
	t.Parallel()
	s, sums, _ := almaPairPublisher(t, 0)
	key := signingKey{name: "AlmaLinux OS 10", armoured: almaLinux10Key, fingerprint: Rocky10KeyFingerprint}
	dir := "https://repo.almalinux.org" + almaFixtureDir
	if _, err := s.signedPair(context.Background(), key, dir+"/CHECKSUM", dir+"/CHECKSUM.asc"); err == nil {
		t.Fatal("a key with another fingerprint verified")
	}
	if got := sums.Load(); got != 1 {
		t.Errorf("%d checksum fetches, want 1", got)
	}
}

func TestVersion3Signatures(t *testing.T) {
	t.Parallel()
	sums := readFixture(t, "leap-16.1/"+leapFixture+".sha256")
	sig := readFixture(t, "leap-16.1/"+leapFixture+".sha256.asc")
	v3, isV3, err := parseV3(sig)
	if err != nil || !isV3 {
		t.Fatalf("openSUSE's signature read as version 3: %v, %v", isV3, err)
	}
	if _, isV3, err := parseV3(readFixture(t, "almalinux-10/CHECKSUM.asc")); err != nil || isV3 {
		t.Errorf("AlmaLinux's version 4 signature read as version 3: %v, %v", isV3, err)
	}
	opensuse := signingKey{name: "openSUSE", armoured: openSUSEKey, fingerprint: OpenSUSEKeyFingerprint}
	if err := opensuse.verify(sums, sig); err != nil {
		t.Fatalf("the genuine checksums fail: %v", err)
	}
	keyring := readKeyring(t, openSUSEKey)
	if err := v3.verify(keyring, sums, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := v3.verify(keyring, append(sums, '\n'), time.Now()); err == nil {
		t.Error("the signature verified over other data")
	}
	if err := v3.verify(readKeyring(t, almaLinux10Key), sums, time.Now()); err == nil || !strings.Contains(err.Error(), "signed by key") {
		t.Errorf("another key verified: %v", err)
	}
	// Made before the key existed: a signature no key of that name could have made.
	early := *v3
	early.created = keyring.PrimaryKey.CreationTime.Add(-time.Hour)
	if err := early.verify(keyring, sums, time.Now()); err == nil {
		t.Error("a signature from before the key verified")
	}
	// One flipped bit in the signature value.
	flipped := *v3
	flipped.rsa = bytes.Clone(v3.rsa)
	flipped.rsa[len(flipped.rsa)-1] ^= 1
	if err := flipped.verify(keyring, sums, time.Now()); err == nil {
		t.Error("a corrupted signature verified")
	}
	// The armour stripped, as some mirrors serve it.
	binary := dearmour(t, sig)
	if _, isV3, err := parseV3(binary); err != nil || !isV3 {
		t.Errorf("the binary signature: %v, %v", isV3, err)
	}
	if err := opensuse.verify(sums, binary); err != nil {
		t.Errorf("the binary signature fails: %v", err)
	}
	for name, bad := range map[string][]byte{
		"two packets":        append(bytes.Clone(binary), binary...),
		"truncated":          binary[:len(binary)-3],
		"not a packet":       []byte("hello"),
		crypto.SHA1.String(): withHash(t, binary, 2),
	} {
		if _, isV3, err := parseV3(bad); isV3 && err == nil {
			t.Errorf("%s: read as a version 3 signature", name)
		}
		if err := opensuse.verify(sums, bad); err == nil {
			t.Errorf("%s: verified", name)
		}
	}
}

func readKeyring(t *testing.T, armoured []byte) *openpgp.Entity {
	t.Helper()
	keyring, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(armoured))
	if err != nil || len(keyring) != 1 {
		t.Fatalf("reading the key: %v", err)
	}
	return keyring[0]
}

func dearmour(t *testing.T, armoured []byte) []byte {
	t.Helper()
	block, err := armor.Decode(bytes.NewReader(armoured))
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(block.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// withHash is a binary version 3 signature with its hash algorithm byte set to algo.
func withHash(t *testing.T, binary []byte, algo byte) []byte {
	t.Helper()
	body, err := signaturePacket(binary)
	if err != nil {
		t.Fatal(err)
	}
	out := bytes.Clone(binary)
	out[len(binary)-len(body)+16] = algo
	return out
}

func TestFindChecksum(t *testing.T) {
	t.Parallel()
	sums := []byte("0cbad23891160603252a08027141baca3056771f0d9089f4e51daa2f59e27996 *a.img\nbad *b.img\n" +
		"SHA256 (c.img) = 0CBAD23891160603252A08027141BACA3056771F0D9089F4E51DAA2F59E27996\n")
	if _, err := findChecksum(sums, "a.img"); err != nil {
		t.Error(err)
	}
	if got, err := findChecksum(sums, "c.img"); err != nil || got != "0cbad23891160603252a08027141baca3056771f0d9089f4e51daa2f59e27996" {
		t.Errorf("the BSD form: %q, %v", got, err)
	}
	if _, err := findChecksum(sums, "b.img"); err == nil {
		t.Error("a malformed checksum was accepted")
	}
	if _, err := findChecksum(sums, "d.img"); err == nil {
		t.Error("a missing file was found")
	}
}

func TestRepositoryKeyChecksTheFingerprint(t *testing.T) {
	t.Parallel()
	s, files := fakePublishers(t)
	k := RepositoryKey{Name: "pkgs.k8s.io", URL: "https://pkgs.k8s.io" + k8sKeyPath, Fingerprints: []string{KubernetesPackagesKeyFingerprint}}
	key, err := s.RepositoryKey(context.Background(), k)
	if err != nil {
		t.Fatal(err)
	}
	if e, err := oneKey([]byte(key)); err != nil || fingerprint(e) != KubernetesPackagesKeyFingerprint {
		t.Errorf("key %.40q: %v", key, err)
	}
	// AlmaLinux's key is a valid key, but not the one pkgs.k8s.io signs with.
	files[k8sKeyPath] = almaLinux10Key
	if _, err := s.RepositoryKey(context.Background(), k); err == nil {
		t.Error("a key with another fingerprint was accepted")
	}
	files[k8sKeyPath] = append(readFixture(t, "pkgs.k8s.io-Release.key"), almaLinux10Key...)
	if _, err := s.RepositoryKey(context.Background(), k); err == nil {
		t.Error("a second key served beside it was accepted")
	}
}

// failingPublisher answers the first failures requests with status, then with the request's body.
func failingPublisher(t *testing.T, failures int32, status int) (*Sources, *atomic.Int32) {
	t.Helper()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) <= failures {
			w.WriteHeader(status)
			return
		}
		_, _ = io.Copy(w, r.Body)
	}))
	t.Cleanup(srv.Close)
	return &Sources{HTTP: srv.Client(), TalosFactoryURL: srv.URL, retryWait: time.Millisecond}, &requests
}

func TestSourcesRetryFromTwoSecondsInFourTries(t *testing.T) {
	t.Parallel()
	if sourcesAttempts != 4 || sourcesFirstWait != 2*time.Second {
		t.Errorf("%d tries from %v, want 4 from 2s", sourcesAttempts, sourcesFirstWait)
	}
}

func TestAPassingFailureIsTriedAgain(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusTooManyRequests, http.StatusRequestTimeout} {
		s, requests := failingPublisher(t, 2, status)
		body, err := s.do(context.Background(), http.MethodPost, s.TalosFactoryURL+"/schematics", []byte(TalosSchematic))
		if err != nil {
			t.Fatalf("%d twice, then 200: %v", status, err)
		}
		if string(body) != TalosSchematic {
			t.Errorf("%d: the third request carried %q, not the whole body", status, body)
		}
		if got := requests.Load(); got != 3 {
			t.Errorf("%d: %d requests, want 3", status, got)
		}
	}
}

func TestALastingFailureIsNotTriedAgain(t *testing.T) {
	t.Parallel()
	for _, status := range []int{http.StatusNotFound, http.StatusForbidden, http.StatusBadRequest} {
		s, requests := failingPublisher(t, 1, status)
		if _, err := s.do(context.Background(), http.MethodGet, s.TalosFactoryURL+"/versions", nil); err == nil {
			t.Errorf("%d was taken as success", status)
		}
		if got := requests.Load(); got != 1 {
			t.Errorf("%d: %d requests, want 1", status, got)
		}
	}
}

func TestRetriesEndAfterTheLastAttempt(t *testing.T) {
	t.Parallel()
	s, requests := failingPublisher(t, sourcesAttempts+1, http.StatusServiceUnavailable)
	_, err := s.do(context.Background(), http.MethodGet, s.TalosFactoryURL+"/versions", nil)
	if err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("error %v, want the last answer's 503", err)
	}
	if got := requests.Load(); got != sourcesAttempts {
		t.Errorf("%d requests, want %d", got, sourcesAttempts)
	}
}

func TestADroppedConnectionIsTriedAgain(t *testing.T) {
	t.Parallel()
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			conn, _, err := http.NewResponseController(w).Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		_, _ = io.WriteString(w, "ok")
	}))
	t.Cleanup(srv.Close)
	s := &Sources{HTTP: srv.Client(), retryWait: time.Millisecond}
	body, err := s.do(context.Background(), http.MethodGet, srv.URL, nil)
	if err != nil || string(body) != "ok" {
		t.Errorf("got %q, %v after a dropped connection, want ok", body, err)
	}
	if got := requests.Load(); got != 2 {
		t.Errorf("%d requests, want 2", got)
	}
}

func TestACancelledBuildStopsRetrying(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		cancel()
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)
	s := &Sources{HTTP: srv.Client(), retryWait: time.Hour}
	done := make(chan error, 1)
	go func() {
		_, err := s.do(ctx, http.MethodGet, srv.URL, nil)
		done <- err
	}()
	if err := <-done; err == nil {
		t.Error("a cancelled build was taken as success")
	}
	if got := requests.Load(); got != 1 {
		t.Errorf("%d requests, want 1", got)
	}
}
