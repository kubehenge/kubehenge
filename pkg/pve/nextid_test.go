package pve

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"
)

func optionsAPI(t *testing.T, options any) *Accounts {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api2/json/cluster/options", func(w http.ResponseWriter, r *http.Request) {
		switch signedIn(r) {
		case nil:
			http.Error(w, "no ticket", http.StatusUnauthorized)
		case bob:
			http.Error(w, "Permission check failed", http.StatusForbidden)
		default:
			replyData(t, w, options)
		}
	})
	mux.HandleFunc("GET /api2/json/cluster/resources", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("type") != "vm" {
			t.Errorf("resources of type %q", r.URL.Query().Get("type"))
		}
		replyData(t, w, []map[string]any{{"vmid": 100, "type": "qemu"}, {"vmid": 205, "type": "lxc"}})
	})
	return serveAPI(t, mux)
}

func TestNextIDReadsTheDatacentersRange(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		options any
		want    NextID
	}{
		"unset":      {map[string]any{"keyboard": "en-us"}, NextID{}},
		"object":     {map[string]any{"next-id": map[string]any{"lower": 5000, "upper": 6000}}, NextID{5000, 6000}},
		"lower only": {map[string]any{"next-id": map[string]any{"lower": 5000}}, NextID{Lower: 5000}},
		"text":       {map[string]any{"next-id": "lower=7000,upper=8000"}, NextID{7000, 8000}},
		"upper text": {map[string]any{"next-id": "upper=8000"}, NextID{Upper: 8000}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got, err := optionsAPI(t, c.options).NextID(context.Background(), alice)
			if err != nil || got != c.want {
				t.Errorf("NextID = %+v, %v, want %+v", got, err, c.want)
			}
		})
	}
}

func TestNextIDNeedsSysAuditOnRoot(t *testing.T) {
	t.Parallel()
	_, err := optionsAPI(t, map[string]any{}).NextID(context.Background(), bob)
	if !errors.Is(err, ErrForbidden) {
		t.Errorf("err = %v, want ErrForbidden", err)
	}
}

func TestVMIDsListContainersToo(t *testing.T) {
	t.Parallel()
	ids, err := optionsAPI(t, nil).VMIDs(context.Background(), alice)
	if err != nil || !reflect.DeepEqual(ids, []int{100, 205}) {
		t.Errorf("VMIDs = %v, %v", ids, err)
	}
}
