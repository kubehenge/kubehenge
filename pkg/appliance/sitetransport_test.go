package appliance

import (
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSavedTransportTrustsTheSiteCA(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	t.Cleanup(server.Close)
	site := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}))

	reach := func(siteCA string) error {
		dir := t.TempDir()
		a := &Appliance{Dir: dir}
		if err := os.MkdirAll(a.workdir(), privateDir); err != nil {
			t.Fatal(err)
		}
		s := settings("pve1")
		s.SiteCA = siteCA
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(filepath.Join(a.workdir(), "settings.json"), raw, privateFile); err != nil {
			t.Fatal(err)
		}
		resp, err := (&http.Client{Transport: &SavedTransport{Dir: dir}, Timeout: 10 * time.Second}).Get(server.URL) //nolint:noctx // a test server
		if err == nil {
			_ = resp.Body.Close()
		}
		return err
	}
	if err := reach(site); err != nil {
		t.Errorf("a server the site CA signs: %v", err)
	}
	if err := reach(""); err == nil {
		t.Error("a server no authority signs was reached without a site CA")
	}
}
