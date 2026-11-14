package backupstore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kubehenge/kubehenge/pkg/provisioner"
)

// The store's place in the management cluster.
const (
	Namespace = "kubehenge-system"
	Service   = "kubehenge-backup-store"
	Port      = "http"
	// TokenSecret holds the store's token under TokenKey.
	TokenSecret = "kubehenge-backup-store" //nolint:gosec // a Secret's name
	TokenKey    = "token"
	// Claim is the backup volume's claim.
	Claim = "kubehenge-backups"
)

// ErrNoPresign is PresignGet's answer: the store hands out no URLs.
var ErrNoPresign = errors.New("the backup volume hands out no download links")

// Client reaches a store at Base, which ends where the store's own paths start.
type Client struct {
	Base  string
	Token string
	HTTP  *http.Client
}

var _ provisioner.ObjectStore = (*Client)(nil)

// ProxyBase is the store's address through the API server's service proxy at host.
func ProxyBase(host string) string {
	return strings.TrimSuffix(host, "/") + "/api/v1/namespaces/" + Namespace + "/services/" + Service + ":" + Port + "/proxy"
}

func (c *Client) do(ctx context.Context, method, rel string, body io.Reader, header http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.Base+rel, body)
	if err != nil {
		return nil, fmt.Errorf("the backup volume: %w", err)
	}
	for k, v := range header {
		req.Header[k] = v
	}
	req.Header.Set(TokenHeader, c.Token)
	client := c.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("the backup volume: %w", err)
	}
	return resp, nil
}

func objectPath(key string) string {
	parts := strings.Split(key, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return "/objects/" + strings.Join(parts, "/")
}

// failed turns a response that is not want into an error, with the store's own text.
func failed(resp *http.Response, key string) error {
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s: %w", key, provisioner.ErrNoObject)
	}
	text, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	return fmt.Errorf("the backup volume answered %s for %s: %s", resp.Status, key, strings.TrimSpace(string(text)))
}

// Put stores r under key.
func (c *Client) Put(ctx context.Context, key string, r io.Reader, metadata map[string]string) (int64, error) {
	if !ValidKey(key) {
		return 0, fmt.Errorf("%q is no object's key", key)
	}
	header := http.Header{}
	if len(metadata) > 0 {
		raw, err := json.Marshal(metadata)
		if err != nil {
			return 0, fmt.Errorf("encoding %s's metadata: %w", key, err)
		}
		header.Set(MetadataHeader, string(raw))
	}
	counted := &counter{r: r}
	resp, err := c.do(ctx, http.MethodPut, objectPath(key), counted, header)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusCreated {
		return 0, failed(resp, key)
	}
	return counted.n, nil
}

type counter struct {
	r io.Reader
	n int64
}

func (c *counter) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err //nolint:wrapcheck // io.Reader's own errors, io.EOF among them
}

// List lists the objects under prefix.
func (c *Client) List(ctx context.Context, prefix string) ([]provisioner.StoredObject, error) {
	resp, err := c.do(ctx, http.MethodGet, "/list?prefix="+url.QueryEscape(prefix), nil, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, failed(resp, prefix)
	}
	var entries []Entry
	if err := json.NewDecoder(resp.Body).Decode(&entries); err != nil {
		return nil, fmt.Errorf("the backup volume's listing: %w", err)
	}
	out := make([]provisioner.StoredObject, 0, len(entries))
	for _, e := range entries {
		out = append(out, provisioner.StoredObject{Key: e.Key, Size: e.Size, Modified: e.Modified})
	}
	return out, nil
}

// Stat reads one object's size, time and metadata.
func (c *Client) Stat(ctx context.Context, key string) (provisioner.StoredObject, error) {
	resp, err := c.do(ctx, http.MethodHead, objectPath(key), nil, nil)
	if err != nil {
		return provisioner.StoredObject{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return provisioner.StoredObject{}, failed(resp, key)
	}
	obj := provisioner.StoredObject{Key: key}
	obj.Size, _ = strconv.ParseInt(resp.Header.Get("Content-Length"), 10, 64)
	obj.Modified, _ = http.ParseTime(resp.Header.Get("Last-Modified"))
	if raw := resp.Header.Get(MetadataHeader); raw != "" {
		if err := json.Unmarshal([]byte(raw), &obj.Metadata); err != nil {
			return obj, fmt.Errorf("%s's metadata: %w", key, err)
		}
	}
	return obj, nil
}

// Get streams an object.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	resp, err := c.do(ctx, http.MethodGet, objectPath(key), nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		return nil, failed(resp, key)
	}
	return resp.Body, nil
}

// Remove deletes an object. One that is gone already is no error.
func (c *Client) Remove(ctx context.Context, key string) error {
	resp, err := c.do(ctx, http.MethodDelete, objectPath(key), nil, nil)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNoContent {
		return failed(resp, key)
	}
	return nil
}

// PresignGet is refused: the volume is reached only through the store.
func (c *Client) PresignGet(context.Context, string, time.Duration) (string, error) {
	return "", ErrNoPresign
}
