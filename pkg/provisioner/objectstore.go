package provisioner

import (
	"cmp"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

const (
	// storePartSize is the part a streamed upload is sent in. The client holds one part in memory at
	// a time, so a snapshot of any size uploads in this much. A part this size allows objects of
	// 80 GiB in the 10,000 parts S3 takes.
	storePartSize = 8 << 20
	// storeDialTimeout and storeResponseTimeout bound reaching the store and its first answer. A
	// transfer itself takes as long as it takes.
	storeDialTimeout     = 10 * time.Second
	storeResponseTimeout = time.Minute
	// awsEndpoint is where a target without an endpoint of its own is: Amazon S3.
	awsEndpoint = "s3.amazonaws.com"
)

// StoredObject is an object in a target, with the metadata it was stored with, by lower-case name.
type StoredObject struct {
	Key      string
	Size     int64
	Modified time.Time
	Metadata map[string]string
}

// ObjectStore is a cluster's way into its backup target's bucket.
type ObjectStore interface {
	// Put streams r into key with metadata and reports the bytes stored.
	Put(ctx context.Context, key string, r io.Reader, metadata map[string]string) (int64, error)
	// SetMetadata replaces the metadata of an object.
	SetMetadata(ctx context.Context, key string, metadata map[string]string) error
	// List lists the objects under prefix, with their metadata.
	List(ctx context.Context, prefix string) ([]StoredObject, error)
	// Stat reads one object's size and metadata.
	Stat(ctx context.Context, key string) (StoredObject, error)
	// Get streams an object.
	Get(ctx context.Context, key string) (io.ReadCloser, error)
	// Remove deletes an object. One that is gone already is no error.
	Remove(ctx context.Context, key string) error
	// PresignGet is a URL anyone may fetch the object with until it expires.
	PresignGet(ctx context.Context, key string, expires time.Duration) (string, error)
}

// ErrNoObject is what an ObjectStore answers for an object that is not there.
var ErrNoObject = errors.New("no such object")

// OpenStore reaches a target with the access key of a backup credential.
type OpenStore func(t *BackupTarget, credential map[string][]byte) (ObjectStore, error)

// OpenTarget reaches a target's bucket over HTTPS with an access key, verifying the store's
// certificate against the system's CAs and the target's own.
func OpenTarget(t *BackupTarget, credential map[string][]byte) (ObjectStore, error) { //nolint:ireturn // an OpenStore, which tests stand in for with stores of their own
	endpoint, lookup := awsEndpoint, minio.BucketLookupAuto
	if t.Endpoint != "" {
		u, err := url.Parse(t.Endpoint)
		if err != nil || !strings.HasPrefix(t.Endpoint, "https://") || u.Host == "" || strings.Trim(u.Path, "/") != "" {
			return nil, fmt.Errorf("target %s: endpoint %q is not an https:// address without a path", t.Name, t.Endpoint)
		}
		// S3-compatible stores take the bucket in the path, as Velero is told to send it.
		endpoint, lookup = u.Host, minio.BucketLookupPath
	}
	transport, err := targetTransport(t)
	if err != nil {
		return nil, err
	}
	id := strings.TrimSpace(string(credential[backupAccessKeyID]))
	secret := strings.TrimSpace(string(credential[backupSecretAccessKey]))
	c, err := minio.New(endpoint, &minio.Options{
		Creds:        credentials.NewStaticV4(id, secret, ""),
		Secure:       true,
		Region:       cmp.Or(t.Region, defaultBackupRegion),
		BucketLookup: lookup,
		Transport:    transport,
	})
	if err != nil {
		return nil, fmt.Errorf("target %s: %w", t.Name, err)
	}
	return &s3Store{client: c, bucket: t.Bucket}, nil
}

// targetTransport reaches a target over HTTPS, verifying its certificate against the system's CAs
// and the target's own.
func targetTransport(t *BackupTarget) (*http.Transport, error) {
	roots, err := x509.SystemCertPool()
	if err != nil {
		roots = x509.NewCertPool()
	}
	if t.CACertificate != "" && !roots.AppendCertsFromPEM([]byte(t.CACertificate)) {
		return nil, fmt.Errorf("target %s: its CA certificate is not PEM", t.Name)
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: storeDialTimeout}).DialContext,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots},
		TLSHandshakeTimeout:   storeDialTimeout,
		ResponseHeaderTimeout: storeResponseTimeout,
		ForceAttemptHTTP2:     true,
	}, nil
}

// serves tells whether a link Velero signed leads to the target itself, the one place the SSE-C
// key sent with it may go.
func (t *BackupTarget) serves(link string) bool {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || u.User != nil {
		return false
	}
	if t.Endpoint == "" {
		return u.Hostname() == awsEndpoint || strings.HasSuffix(u.Hostname(), ".amazonaws.com")
	}
	e, err := url.Parse(t.Endpoint)
	return err == nil && strings.EqualFold(e.Host, u.Host)
}

type s3Store struct {
	client *minio.Client
	bucket string
}

func (s *s3Store) Put(ctx context.Context, key string, r io.Reader, metadata map[string]string) (int64, error) {
	info, err := s.client.PutObject(ctx, s.bucket, key, r, -1, minio.PutObjectOptions{
		UserMetadata: metadata, ContentType: "application/octet-stream", PartSize: storePartSize,
	})
	if err != nil {
		// A multipart upload cut short keeps its parts, which the store charges for, until removed.
		if rerr := s.client.RemoveIncompleteUpload(context.WithoutCancel(ctx), s.bucket, key); rerr != nil {
			err = errors.Join(err, rerr)
		}
		return 0, fmt.Errorf("storing %s: %w", key, err)
	}
	return info.Size, nil
}

func (s *s3Store) SetMetadata(ctx context.Context, key string, metadata map[string]string) error {
	_, err := s.client.CopyObject(ctx,
		minio.CopyDestOptions{Bucket: s.bucket, Object: key, ReplaceMetadata: true, UserMetadata: metadata},
		minio.CopySrcOptions{Bucket: s.bucket, Object: key})
	if err != nil {
		return fmt.Errorf("setting the metadata of %s: %w", key, err)
	}
	return nil
}

func (s *s3Store) List(ctx context.Context, prefix string) ([]StoredObject, error) {
	var out []StoredObject
	for o := range s.client.ListObjectsIter(ctx, s.bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if o.Err != nil {
			return nil, fmt.Errorf("listing %s: %w", prefix, o.Err)
		}
		// A listing carries no user metadata. Each object's is read on its own.
		st, err := s.Stat(ctx, o.Key)
		if errors.Is(err, ErrNoObject) {
			continue
		}
		if err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, nil
}

func (s *s3Store) Stat(ctx context.Context, key string) (StoredObject, error) {
	info, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{})
	if err != nil {
		if minio.ToErrorResponse(err).StatusCode == http.StatusNotFound {
			return StoredObject{}, fmt.Errorf("%s: %w", key, ErrNoObject)
		}
		return StoredObject{}, fmt.Errorf("reading %s: %w", key, err)
	}
	meta := make(map[string]string, len(info.UserMetadata))
	for k, v := range info.UserMetadata {
		meta[strings.ToLower(k)] = v
	}
	return StoredObject{Key: info.Key, Size: info.Size, Modified: info.LastModified, Metadata: meta}, nil
}

func (s *s3Store) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if _, err := s.Stat(ctx, key); err != nil {
		return nil, err
	}
	o, err := s.client.GetObject(ctx, s.bucket, key, minio.GetObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", key, err)
	}
	return o, nil
}

func (s *s3Store) Remove(ctx context.Context, key string) error {
	if err := s.client.RemoveObject(ctx, s.bucket, key, minio.RemoveObjectOptions{}); err != nil {
		return fmt.Errorf("deleting %s: %w", key, err)
	}
	return nil
}

func (s *s3Store) PresignGet(ctx context.Context, key string, expires time.Duration) (string, error) {
	u, err := s.client.PresignedGetObject(ctx, s.bucket, key, expires, nil)
	if err != nil {
		return "", fmt.Errorf("signing a link to %s: %w", key, err)
	}
	return u.String(), nil
}
