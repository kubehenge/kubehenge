package templates

import (
	"bytes"
	"context"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// The fixtures are Ubuntu's real signed checksums of the 26.04 cloud images, and pkgs.k8s.io's key.
func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := fs.ReadFile(os.DirFS("testdata"), name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func fakePublishers(t *testing.T) *Sources {
	t.Helper()
	sums := readFixture(t, "ubuntu-26.04/SHA256SUMS")
	sig := readFixture(t, "ubuntu-26.04/SHA256SUMS.gpg")
	key := readFixture(t, "pkgs.k8s.io-Release.key")
	mux := http.NewServeMux()
	mux.HandleFunc("POST /schematics", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if string(body) != TalosSchematic {
			http.Error(w, "unexpected schematic", http.StatusBadRequest)
			return
		}
		_, _ = io.WriteString(w, `{"id":"ce4c980550dd2ab1b17bbf2b08801c7eb59418eafe8f279833297925d67c7515"}`)
	})
	mux.HandleFunc("GET /releases/resolute/release/SHA256SUMS", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(sums) })
	mux.HandleFunc("GET /releases/resolute/release/SHA256SUMS.gpg", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(sig) })
	mux.HandleFunc("GET /core:/stable:/v1.37/deb/Release.key", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(key) })
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Sources{HTTP: srv.Client(), TalosFactoryURL: srv.URL, UbuntuCloudURL: srv.URL + "/releases", KubernetesPkgs: srv.URL}
}

func TestResolveTalos(t *testing.T) {
	t.Parallel()
	s := fakePublishers(t)
	got, err := s.Resolve(context.Background(), ImageRef{Kind: TalosFactory, Version: "v1.14.2"})
	if err != nil {
		t.Fatal(err)
	}
	want := Resolved{
		URL:      s.TalosFactoryURL + "/image/ce4c980550dd2ab1b17bbf2b08801c7eb59418eafe8f279833297925d67c7515/v1.14.2/nocloud-amd64.qcow2",
		FileName: "template-talos-v1.14.2-ce4c980550dd-nocloud-amd64.qcow2",
	}
	if got != want {
		t.Errorf("Resolve() = %+v, want %+v", got, want)
	}
}

func TestResolveUbuntuVerifiesTheSignedChecksums(t *testing.T) {
	t.Parallel()
	s := fakePublishers(t)
	got, err := s.Resolve(context.Background(), ImageRef{Kind: UbuntuCloud, Version: "ubuntu-26.04"})
	if err != nil {
		t.Fatal(err)
	}
	want := Resolved{
		URL:               s.UbuntuCloudURL + "/resolute/release/ubuntu-26.04-server-cloudimg-amd64.img",
		FileName:          "template-ubuntu-26.04-8800651811af.qcow2",
		Checksum:          "8800651811af9a85465ad1d552add729947bb16488dddb4a9b5305a3d97332b2",
		ChecksumAlgorithm: "sha256",
	}
	if got != want {
		t.Errorf("Resolve() = %+v, want %+v", got, want)
	}
}

func TestVerifyUbuntuChecksumsRejectsTampering(t *testing.T) {
	t.Parallel()
	sums := readFixture(t, "ubuntu-26.04/SHA256SUMS")
	sig := readFixture(t, "ubuntu-26.04/SHA256SUMS.gpg")
	if err := verifyUbuntuChecksums(sums, sig); err != nil {
		t.Fatalf("the genuine checksums fail: %v", err)
	}
	tampered := bytes.Replace(sums, []byte("8800651811af"), []byte("0000000000af"), 1)
	if err := verifyUbuntuChecksums(tampered, sig); err == nil {
		t.Error("a tampered checksum list verified")
	}
	if err := verifyUbuntuChecksums(sums, []byte("-----BEGIN PGP SIGNATURE-----\n\n-----END PGP SIGNATURE-----\n")); err == nil {
		t.Error("an empty signature verified")
	}
}

func TestFindChecksum(t *testing.T) {
	t.Parallel()
	sums := []byte("0cbad23891160603252a08027141baca3056771f0d9089f4e51daa2f59e27996 *a.img\nbad *b.img\n")
	if _, err := findChecksum(sums, "a.img"); err != nil {
		t.Error(err)
	}
	if _, err := findChecksum(sums, "b.img"); err == nil {
		t.Error("a malformed checksum was accepted")
	}
	if _, err := findChecksum(sums, "c.img"); err == nil {
		t.Error("a missing file was found")
	}
}

func TestKubernetesPackagesKeyChecksTheFingerprint(t *testing.T) {
	t.Parallel()
	s := fakePublishers(t)
	key, err := s.KubernetesPackagesKey(context.Background(), "v1.37")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "-----BEGIN PGP PUBLIC KEY BLOCK-----") {
		t.Errorf("key is not ASCII-armoured: %.40q", key)
	}

	// Ubuntu's key is a valid key, but not the one pkgs.k8s.io signs with.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(ubuntuSigningKey) }))
	defer srv.Close()
	s.KubernetesPkgs = srv.URL
	if _, err := s.KubernetesPackagesKey(context.Background(), "v1.37"); err == nil {
		t.Error("a key with another fingerprint was accepted")
	}
}
