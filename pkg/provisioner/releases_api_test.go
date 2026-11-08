package provisioner

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

const apiTestVersion = "v1.37.1+rke2r1"

// downloadsDown serves a release as GitHub does while its download address answers with the status
// and its API still works. The counter holds the API's requests.
func downloadsDown(t *testing.T, assets map[string]string, status int) (*ReleaseChecksums, *atomic.Int32) {
	t.Helper()
	var apiCalls atomic.Int32
	repository := "/" + rke2Release.repository
	mux := http.NewServeMux()
	mux.HandleFunc(repository+"/releases/download/", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
	})
	mux.HandleFunc(repository+"/"+apiTestVersion+"/install.sh", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("#!/bin/sh\n"))
	})
	mux.HandleFunc("/api/repos"+repository+"/releases/tags/"+apiTestVersion, func(w http.ResponseWriter, _ *http.Request) {
		apiCalls.Add(1)
		var b strings.Builder
		b.WriteString(`{"assets":[{"id":7,"name":"other"}`)
		if _, ok := assets["sha256sum-amd64.txt"]; ok {
			b.WriteString(`,{"id":42,"name":"sha256sum-amd64.txt"}`)
		}
		b.WriteString(`]}`)
		_, _ = w.Write([]byte(b.String()))
	})
	mux.HandleFunc("/api/repos"+repository+"/releases/assets/42", func(w http.ResponseWriter, r *http.Request) {
		apiCalls.Add(1)
		if r.Header.Get("Accept") != "application/octet-stream" {
			http.Error(w, "wrong Accept", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(assets["sha256sum-amd64.txt"]))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &ReleaseChecksums{HTTP: srv.Client(), GitHub: srv.URL, Raw: srv.URL, API: srv.URL + "/api"}, &apiCalls
}

func TestReleaseChecksumsReadThroughTheAPIWhenDownloadsFail(t *testing.T) {
	t.Parallel()
	r, calls := downloadsDown(t, map[string]string{"sha256sum-amd64.txt": rke2Listing}, http.StatusGatewayTimeout)
	sums, err := r.read(context.Background(), rke2Release, apiTestVersion)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if sums.File != hexSHA256([]byte(rke2Listing)) {
		t.Errorf("the checksum file's hash is %s, not that of the API's file", sums.File)
	}
	if calls.Load() != 2 {
		t.Errorf("the API got %d requests, want the release's and the asset's", calls.Load())
	}
}

func TestReleaseChecksumsDoNotAskTheAPIForAFileThatIsNotThere(t *testing.T) {
	t.Parallel()
	r, calls := downloadsDown(t, map[string]string{"sha256sum-amd64.txt": rke2Listing}, http.StatusNotFound)
	_, err := r.read(context.Background(), rke2Release, apiTestVersion)
	if !errors.Is(err, errUnlisted) {
		t.Fatalf("read: %v, want errUnlisted", err)
	}
	if calls.Load() != 0 {
		t.Errorf("the API got %d requests for a file the download address says is not there", calls.Load())
	}
}

func TestReleaseChecksumsReportAnAssetTheReleaseLacks(t *testing.T) {
	t.Parallel()
	r, _ := downloadsDown(t, map[string]string{}, http.StatusBadGateway)
	_, err := r.read(context.Background(), rke2Release, apiTestVersion)
	if !errors.Is(err, errUnlisted) || !strings.Contains(err.Error(), "502") {
		t.Fatalf("read: %v, want errUnlisted naming the 502", err)
	}
}

func TestReleaseChecksumsKeepTheDownloadErrorWithoutAnAPI(t *testing.T) {
	t.Parallel()
	r, calls := downloadsDown(t, map[string]string{"sha256sum-amd64.txt": rke2Listing}, http.StatusGatewayTimeout)
	r.API = ""
	_, err := r.read(context.Background(), rke2Release, apiTestVersion)
	if err == nil || !strings.Contains(err.Error(), "504") || errors.Is(err, errUnlisted) {
		t.Fatalf("read: %v, want the 504", err)
	}
	if calls.Load() != 0 {
		t.Errorf("the API got %d requests though none is set", calls.Load())
	}
}
