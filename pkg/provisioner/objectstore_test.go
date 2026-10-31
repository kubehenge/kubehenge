package provisioner

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/md5" //nolint:gosec // S3's ETag is an MD5, which the fake store computes as S3 does
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"maps"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"
	"time"

	"github.com/minio/minio-go/v7/pkg/s3utils"
)

const (
	fakeS3Bucket = "backups"
	fakeS3ID     = "AKIAKUBEHENGE"
	fakeS3Secret = "s3cret/key+value"
	// fakeS3PageSize is S3's own cap on a listing page, whatever max-keys asks for.
	fakeS3PageSize = 1000
)

type fakeObject struct {
	data     []byte
	meta     http.Header
	modified time.Time
}

type fakeUpload struct {
	key   string
	meta  http.Header
	parts map[int][]byte
}

// The S3 documents the fake store reads and writes.
type (
	s3ListEntry struct {
		Key          string `xml:"Key"`
		LastModified string `xml:"LastModified"`
		ETag         string `xml:"ETag"`
		Size         int    `xml:"Size"`
		StorageClass string `xml:"StorageClass"`
	}
	s3Listing struct {
		XMLName               xml.Name      `xml:"ListBucketResult"`
		Name                  string        `xml:"Name"`
		Prefix                string        `xml:"Prefix"`
		KeyCount              int           `xml:"KeyCount"`
		MaxKeys               int           `xml:"MaxKeys"`
		EncodingType          string        `xml:"EncodingType"`
		IsTruncated           bool          `xml:"IsTruncated"`
		NextContinuationToken string        `xml:"NextContinuationToken,omitempty"`
		Contents              []s3ListEntry `xml:"Contents"`
	}
	s3Upload struct {
		Key       string `xml:"Key"`
		UploadID  string `xml:"UploadId"`
		Initiated string `xml:"Initiated"`
	}
	s3Uploads struct {
		XMLName     xml.Name   `xml:"ListMultipartUploadsResult"`
		Bucket      string     `xml:"Bucket"`
		Prefix      string     `xml:"Prefix"`
		IsTruncated bool       `xml:"IsTruncated"`
		Uploads     []s3Upload `xml:"Upload"`
	}
	s3Initiated struct {
		XMLName  xml.Name `xml:"InitiateMultipartUploadResult"`
		Bucket   string   `xml:"Bucket"`
		Key      string   `xml:"Key"`
		UploadID string   `xml:"UploadId"`
	}
	s3Part struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	}
	s3Complete struct {
		Parts []s3Part `xml:"Part"`
	}
	s3Completed struct {
		XMLName xml.Name `xml:"CompleteMultipartUploadResult"`
		Bucket  string   `xml:"Bucket"`
		Key     string   `xml:"Key"`
		ETag    string   `xml:"ETag"`
	}
	s3Copied struct {
		XMLName      xml.Name `xml:"CopyObjectResult"`
		LastModified string   `xml:"LastModified"`
		ETag         string   `xml:"ETag"`
	}
	s3Failure struct {
		XMLName xml.Name `xml:"Error"`
		Code    string   `xml:"Code"`
		Message string   `xml:"Message"`
	}
)

// fakeS3 is the subset of S3 the store speaks, path-style: multipart uploads, copies that
// replace metadata, ListObjectsV2, HEAD, GET and DELETE. It checks every request's SigV4
// signature, a presigned link's included, so a request it serves was signed with the key it knows.
type fakeS3 struct {
	t      *testing.T
	region string

	mu       sync.Mutex
	objects  map[string]fakeObject
	uploads  map[string]*fakeUpload
	uploadID int
	// vanished keys are listed but gone by the time they are read, as when a snapshot is pruned
	// between the two.
	vanished    map[string]bool
	lists       int
	parts       int
	aborts      int
	abortRefuse int
	encrypted   []string
}

func newFakeS3(t *testing.T, region string) (*fakeS3, *httptest.Server) {
	t.Helper()
	f := &fakeS3{t: t, region: region, objects: map[string]fakeObject{}, uploads: map[string]*fakeUpload{}, vanished: map[string]bool{}}
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	return f, srv
}

func certificatePEM(srv *httptest.Server) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}))
}

// openFake opens the fake store as a target, its address and CA as an administrator gives them and
// the credential as a Secret holds it, with the line breaks such Secrets often end in.
func openFake(t *testing.T, srv *httptest.Server, region string) ObjectStore { //nolint:ireturn // the store under test, by the interface its callers use
	t.Helper()
	store, err := OpenTarget(&BackupTarget{Name: "offsite", Endpoint: srv.URL, Bucket: fakeS3Bucket, Region: region, CACertificate: certificatePEM(srv)},
		map[string][]byte{backupAccessKeyID: []byte(" " + fakeS3ID + "\n"), backupSecretAccessKey: []byte(fakeS3Secret + "\n")})
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if err := f.verify(r); err != nil {
		s3Error(w, http.StatusForbidden, "SignatureDoesNotMatch", err.Error())
		return
	}
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != fakeS3Bucket {
		s3Error(w, http.StatusNotFound, "NoSuchBucket", bucket)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		f.t.Error(err)
		return
	}
	q := r.URL.Query()
	f.mu.Lock()
	defer f.mu.Unlock()
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-amz-server-side-encryption") {
			f.encrypted = append(f.encrypted, r.Method+" "+r.URL.Path+" "+name)
		}
	}
	switch {
	case key == "" && r.Method == http.MethodGet && q.Get("list-type") == "2":
		f.list(w, q)
	case key == "" && r.Method == http.MethodGet && q.Has("uploads"):
		f.listUploads(w, q.Get("prefix"))
	case key == "":
		f.t.Errorf("unexpected %s on the bucket, %s", r.Method, r.URL.RawQuery)
		s3Error(w, http.StatusNotImplemented, "NotImplemented", r.URL.RawQuery)
	case q.Has("uploads") || q.Has("uploadId"):
		f.serveUpload(w, r, key, body)
	default:
		f.serveObject(w, r, key)
	}
}

func (f *fakeS3) put(key string, data []byte, meta map[string]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	h := http.Header{}
	for k, v := range meta {
		h.Set("X-Amz-Meta-"+k, v)
	}
	f.objects[key] = fakeObject{data: data, meta: h, modified: time.Now().UTC().Truncate(time.Second)}
}

func (f *fakeS3) serveUpload(w http.ResponseWriter, r *http.Request, key string, body []byte) {
	q := r.URL.Query()
	id := q.Get("uploadId")
	switch {
	case r.Method == http.MethodPost && q.Has("uploads"):
		f.uploadID++
		id = strconv.Itoa(f.uploadID)
		f.uploads[id] = &fakeUpload{key: key, meta: userMetadata(r.Header), parts: map[int][]byte{}}
		writeXML(w, s3Initiated{Bucket: fakeS3Bucket, Key: key, UploadID: id})
	case r.Method == http.MethodPut:
		u, ok := f.uploads[id]
		n, err := strconv.Atoi(q.Get("partNumber"))
		if !ok || u.key != key || err != nil {
			s3Error(w, http.StatusNotFound, "NoSuchUpload", id)
			return
		}
		f.parts++
		u.parts[n] = body
		w.Header().Set("ETag", etag(body))
	case r.Method == http.MethodPost:
		f.complete(w, key, id, body)
	case r.Method == http.MethodDelete:
		f.aborts++
		if f.abortRefuse > 0 {
			f.abortRefuse--
			s3Error(w, http.StatusForbidden, "AccessDenied", "abort refused")
			return
		}
		if _, ok := f.uploads[id]; !ok {
			s3Error(w, http.StatusNotFound, "NoSuchUpload", id)
			return
		}
		delete(f.uploads, id)
		w.WriteHeader(http.StatusNoContent)
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL)
		s3Error(w, http.StatusNotImplemented, "NotImplemented", r.Method)
	}
}

func (f *fakeS3) serveObject(w http.ResponseWriter, r *http.Request, key string) {
	switch {
	case r.Method == http.MethodPut && r.Header.Get("X-Amz-Copy-Source") != "":
		f.copy(w, r, key)
	case r.Method == http.MethodHead || r.Method == http.MethodGet:
		o, ok := f.objects[key]
		if !ok {
			s3Error(w, http.StatusNotFound, "NoSuchKey", key)
			return
		}
		h := w.Header()
		maps.Copy(h, o.meta)
		h.Set("Content-Length", strconv.Itoa(len(o.data)))
		h.Set("Content-Type", "application/octet-stream")
		h.Set("Last-Modified", o.modified.Format(http.TimeFormat))
		h.Set("ETag", etag(o.data))
		if r.Method == http.MethodGet {
			_, _ = w.Write(o.data)
		}
	case r.Method == http.MethodDelete:
		// S3 answers a delete of a key that is not there as one that was.
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		f.t.Errorf("unexpected %s %s", r.Method, r.URL)
		s3Error(w, http.StatusNotImplemented, "NotImplemented", r.Method)
	}
}

func (f *fakeS3) list(w http.ResponseWriter, q url.Values) {
	f.lists++
	prefix := q.Get("prefix")
	after := ""
	if token := q.Get("continuation-token"); token != "" {
		// The token is opaque to the client, as S3's are.
		raw, err := base64.URLEncoding.DecodeString(token)
		if err != nil {
			s3Error(w, http.StatusBadRequest, "InvalidArgument", "continuation token")
			return
		}
		after = string(raw)
	}
	page := fakeS3PageSize
	if n, err := strconv.Atoi(q.Get("max-keys")); err == nil && n > 0 && n < page {
		page = n
	}
	var keys []string
	for k := range f.objects {
		keys = append(keys, k)
	}
	for k := range f.vanished {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	res := s3Listing{Name: fakeS3Bucket, Prefix: prefix, MaxKeys: page, EncodingType: "url"}
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) || k <= after {
			continue
		}
		if len(res.Contents) == page {
			res.IsTruncated = true
			res.NextContinuationToken = base64.URLEncoding.EncodeToString([]byte(res.Contents[page-1].Key))
			break
		}
		o := f.objects[k]
		res.Contents = append(res.Contents, s3ListEntry{Key: k, LastModified: time.Now().UTC().Format(time.RFC3339), ETag: etag(o.data), Size: len(o.data), StorageClass: "STANDARD"})
	}
	res.KeyCount = len(res.Contents)
	for i := range res.Contents {
		// The client asks for encoding-type=url and decodes each key.
		res.Contents[i].Key = url.QueryEscape(res.Contents[i].Key)
	}
	writeXML(w, res)
}

func (f *fakeS3) listUploads(w http.ResponseWriter, prefix string) {
	res := s3Uploads{Bucket: fakeS3Bucket, Prefix: prefix}
	for _, id := range slices.Sorted(maps.Keys(f.uploads)) {
		if u := f.uploads[id]; strings.HasPrefix(u.key, prefix) {
			res.Uploads = append(res.Uploads, s3Upload{Key: u.key, UploadID: id, Initiated: time.Now().UTC().Format(time.RFC3339)})
		}
	}
	writeXML(w, res)
}

func (f *fakeS3) complete(w http.ResponseWriter, key, id string, body []byte) {
	u, ok := f.uploads[id]
	if !ok || u.key != key {
		s3Error(w, http.StatusNotFound, "NoSuchUpload", id)
		return
	}
	var req s3Complete
	if err := xml.Unmarshal(body, &req); err != nil || len(req.Parts) == 0 {
		s3Error(w, http.StatusBadRequest, "MalformedXML", fmt.Sprint(err))
		return
	}
	var data []byte
	for i, p := range req.Parts {
		part, ok := u.parts[p.PartNumber]
		if !ok || p.PartNumber != i+1 || strings.Trim(p.ETag, `"`) != strings.Trim(etag(part), `"`) {
			s3Error(w, http.StatusBadRequest, "InvalidPart", strconv.Itoa(p.PartNumber))
			return
		}
		data = append(data, part...)
	}
	delete(f.uploads, id)
	f.objects[key] = fakeObject{data: data, meta: u.meta, modified: time.Now().UTC().Truncate(time.Second)}
	writeXML(w, s3Completed{Bucket: fakeS3Bucket, Key: key, ETag: etag(data)})
}

func (f *fakeS3) copy(w http.ResponseWriter, r *http.Request, key string) {
	source, err := url.PathUnescape(r.Header.Get("X-Amz-Copy-Source"))
	bucket, from, _ := strings.Cut(strings.TrimPrefix(source, "/"), "/")
	o, ok := f.objects[from]
	if err != nil || bucket != fakeS3Bucket || !ok {
		s3Error(w, http.StatusNotFound, "NoSuchKey", source)
		return
	}
	if r.Header.Get("X-Amz-Metadata-Directive") == "REPLACE" {
		o.meta = userMetadata(r.Header)
	} else if from == key {
		s3Error(w, http.StatusBadRequest, "InvalidRequest", "copying an object onto itself without changing it")
		return
	}
	o.modified = time.Now().UTC().Truncate(time.Second)
	f.objects[key] = o
	writeXML(w, s3Copied{LastModified: o.modified.Format(time.RFC3339), ETag: etag(o.data)})
}

// verify checks a request's SigV4 signature as S3 does, from the Authorization header or, for a
// presigned link, from the query.
func (f *fakeS3) verify(r *http.Request) error {
	q := r.URL.Query()
	var credential, signed, signature, date, payload string
	if q.Has("X-Amz-Signature") {
		credential, signed, signature, date = q.Get("X-Amz-Credential"), q.Get("X-Amz-SignedHeaders"), q.Get("X-Amz-Signature"), q.Get("X-Amz-Date")
		payload = "UNSIGNED-PAYLOAD"
		issued, err := time.Parse("20060102T150405Z", date)
		expires, eerr := strconv.Atoi(q.Get("X-Amz-Expires"))
		if err != nil || eerr != nil || time.Now().After(issued.Add(time.Duration(expires)*time.Second)) {
			return errors.New("the link has expired")
		}
		q.Del("X-Amz-Signature")
	} else {
		auth, ok := strings.CutPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 ")
		if !ok {
			return errors.New("the request is not signed")
		}
		for field := range strings.SplitSeq(auth, ",") {
			k, v, _ := strings.Cut(strings.TrimSpace(field), "=")
			switch k {
			case "Credential":
				credential = v
			case "SignedHeaders":
				signed = v
			case "Signature":
				signature = v
			}
		}
		date, payload = r.Header.Get("X-Amz-Date"), r.Header.Get("X-Amz-Content-Sha256")
	}
	id, scope, _ := strings.Cut(credential, "/")
	scopeParts := strings.Split(scope, "/")
	if id != fakeS3ID || len(scopeParts) != 4 || scopeParts[1] != f.region || scopeParts[2] != "s3" {
		return fmt.Errorf("credential %q is not %s's in %s", credential, fakeS3ID, f.region)
	}
	var headers strings.Builder
	for name := range strings.SplitSeq(signed, ";") {
		value := r.Host
		if name != "host" {
			var values []string
			for _, v := range r.Header.Values(name) {
				values = append(values, strings.Join(strings.Fields(v), " "))
			}
			value = strings.Join(values, ",")
		}
		headers.WriteString(name + ":" + value + "\n")
	}
	canonical := strings.Join([]string{
		r.Method, s3utils.EncodePath(r.URL.Path), strings.ReplaceAll(q.Encode(), "+", "%20"), headers.String(), signed, payload,
	}, "\n")
	digest := sha256.Sum256([]byte(canonical))
	key := []byte("AWS4" + fakeS3Secret)
	for _, part := range scopeParts {
		key = hmacSHA256(key, part)
	}
	want := hex.EncodeToString(hmacSHA256(key, "AWS4-HMAC-SHA256\n"+date+"\n"+scope+"\n"+hex.EncodeToString(digest[:])))
	if !hmac.Equal([]byte(want), []byte(signature)) {
		return errors.New("the signature does not match")
	}
	return nil
}

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	_, _ = h.Write([]byte(data))
	return h.Sum(nil)
}

func userMetadata(h http.Header) http.Header {
	meta := http.Header{}
	for k, v := range h {
		if strings.HasPrefix(k, "X-Amz-Meta-") {
			meta[k] = v
		}
	}
	return meta
}

func etag(data []byte) string {
	sum := md5.Sum(data) //nolint:gosec // S3's ETag is an MD5
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

func writeXML(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/xml")
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(v)
}

func s3Error(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, xml.Header)
	_ = xml.NewEncoder(w).Encode(s3Failure{Code: code, Message: message})
}

func TestOpenTargetRefusesAnEndpointThatIsNotAnHTTPSAddressWithoutAPath(t *testing.T) {
	t.Parallel()
	for _, endpoint := range []string{"http://s3.example.org", "s3.example.org", "https://", "https://s3.example.org/backups", "https://s3.example.org/%zz"} {
		_, err := OpenTarget(&BackupTarget{Name: "offsite", Endpoint: endpoint, Bucket: fakeS3Bucket}, nil)
		if err == nil || !strings.Contains(err.Error(), "is not an https:// address without a path") {
			t.Errorf("endpoint %q: %v", endpoint, err)
		}
	}
	// A trailing slash is no path.
	if _, err := OpenTarget(&BackupTarget{Name: "offsite", Endpoint: "https://s3.example.org:9000/", Bucket: fakeS3Bucket}, nil); err != nil {
		t.Errorf("endpoint with a trailing slash: %v", err)
	}
	// Without an endpoint the target is Amazon S3; opening it sends nothing.
	if _, err := OpenTarget(&BackupTarget{Name: "aws", Bucket: fakeS3Bucket}, nil); err != nil {
		t.Errorf("Amazon S3 target: %v", err)
	}
}

func TestOpenTargetRefusesACACertificateThatIsNotPEM(t *testing.T) {
	t.Parallel()
	_, err := OpenTarget(&BackupTarget{Name: "offsite", Endpoint: "https://s3.example.org", Bucket: fakeS3Bucket, CACertificate: "MIIB not a PEM block"}, nil)
	if err == nil || !strings.Contains(err.Error(), "CA certificate is not PEM") {
		t.Errorf("got %v", err)
	}
}

func TestStoreKeepsAnObjectWithItsMetadataUnderLowerCaseNames(t *testing.T) {
	t.Parallel()
	f, srv := newFakeS3(t, defaultBackupRegion)
	store := openFake(t, srv, "")
	ctx := context.Background()
	// A key with a space and a plus, which the path, the listing and the signature each encode.
	key := "kubehenge/web/etcd/2026-10-29 snap+1.db.age"
	data := []byte("an age-sealed etcd snapshot")
	stored, err := store.Put(ctx, key, bytes.NewReader(data), map[string]string{"Kubernetes-Version": "v1.34.1", "distribution": "rke2"})
	if err != nil {
		t.Fatal(err)
	}
	if stored != int64(len(data)) {
		t.Errorf("stored %d bytes, want %d", stored, len(data))
	}
	o, err := store.Stat(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"kubernetes-version": "v1.34.1", "distribution": "rke2"}
	if o.Key != key || o.Size != int64(len(data)) || !maps.Equal(o.Metadata, want) || time.Since(o.Modified) > time.Minute {
		t.Errorf("stat %+v, want %s of %d bytes with %v", o, key, len(data), want)
	}
	r, err := store.Get(ctx, key)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(r)
	_ = r.Close()
	if err != nil || !bytes.Equal(got, data) {
		t.Errorf("read %q, %v", got, err)
	}
	objects, err := store.List(ctx, "kubehenge/web/")
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) != 1 || objects[0].Key != key || !maps.Equal(objects[0].Metadata, want) {
		t.Errorf("listed %+v", objects)
	}
	// Snapshots are sealed before they leave; the store is never asked to encrypt, which a
	// presigned link could not then read.
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.encrypted) != 0 {
		t.Errorf("requests asked the store to encrypt: %v", f.encrypted)
	}
}

func TestStoreSignsInTheTargetsRegion(t *testing.T) {
	t.Parallel()
	f, srv := newFakeS3(t, "eu-central-2")
	f.put("a", []byte("x"), nil)
	if _, err := openFake(t, srv, "eu-central-2").Stat(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	// The fake store checks the region of every signature.
	if _, err := openFake(t, srv, "").Stat(context.Background(), "a"); err == nil || errors.Is(err, ErrNoObject) {
		t.Errorf("a request signed for %s was taken in eu-central-2: %v", defaultBackupRegion, err)
	}
}

func TestStoreStreamsAnObjectLongerThanOnePart(t *testing.T) {
	t.Parallel()
	f, srv := newFakeS3(t, defaultBackupRegion)
	store := openFake(t, srv, "")
	data := make([]byte, storePartSize+4096)
	for i := range data {
		data[i] = byte(i * 7)
	}
	// A snapshot's length is not known while it streams; the reader hides it.
	stored, err := store.Put(context.Background(), "big", iotest.HalfReader(bytes.NewReader(data)), nil)
	if err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	parts := f.parts
	f.mu.Unlock()
	if stored != int64(len(data)) || parts != 2 {
		t.Errorf("stored %d bytes in %d parts, want %d in 2", stored, parts, len(data))
	}
	r, err := store.Get(context.Background(), "big")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil || sha256.Sum256(got) != sha256.Sum256(data) {
		t.Errorf("read back %d bytes that differ, %v", len(got), err)
	}
}

// The store refuses the client's own abort of the failed upload, so the upload is gone only if Put
// removes it itself.
func TestFailedPutLeavesNoUploadBehind(t *testing.T) {
	t.Parallel()
	f, srv := newFakeS3(t, defaultBackupRegion)
	f.abortRefuse = 1
	store := openFake(t, srv, "")
	broken := errors.New("etcd went away")
	_, err := store.Put(context.Background(), "snap", iotest.ErrReader(broken), nil)
	if !errors.Is(err, broken) {
		t.Fatalf("got %v, want the reader's error", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.uploads) != 0 || len(f.objects) != 0 {
		t.Errorf("left %d uploads and %d objects", len(f.uploads), len(f.objects))
	}
	if f.aborts != 2 {
		t.Errorf("%d aborts, want the client's refused one and the store's own", f.aborts)
	}
}

func TestSetMetadataReplacesTheMetadataAndKeepsTheData(t *testing.T) {
	t.Parallel()
	f, srv := newFakeS3(t, defaultBackupRegion)
	f.put("snap", []byte("sealed"), map[string]string{"kubernetes-version": "v1.34.1", "partial": "yes"})
	store := openFake(t, srv, "")
	ctx := context.Background()
	if err := store.SetMetadata(ctx, "snap", map[string]string{"kubernetes-version": "v1.34.1", "sha256": "abc123"}); err != nil {
		t.Fatal(err)
	}
	o, err := store.Stat(ctx, "snap")
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"kubernetes-version": "v1.34.1", "sha256": "abc123"}; !maps.Equal(o.Metadata, want) || o.Size != int64(len("sealed")) {
		t.Errorf("after setting metadata %+v, want %v and the data kept", o, want)
	}
	if err := store.SetMetadata(ctx, "missing", nil); err == nil {
		t.Error("set the metadata of an object that is not there")
	}
}

func TestListPagesThroughMoreThanAThousandObjects(t *testing.T) {
	t.Parallel()
	f, srv := newFakeS3(t, defaultBackupRegion)
	const n = fakeS3PageSize + 5
	want := make([]string, 0, n)
	for i := range n {
		key := fmt.Sprintf("web/etcd/%05d", i)
		f.put(key, []byte(key), map[string]string{"etcd-revision": strconv.Itoa(i)})
		want = append(want, key)
	}
	f.put("web/etcd", []byte("not under the folder"), nil)
	f.put("other/etcd/00000", []byte("another cluster's"), nil)
	f.vanished["web/etcd/00002-pruned"] = true
	objects, err := openFake(t, srv, "").List(context.Background(), "web/etcd/")
	if err != nil {
		t.Fatal(err)
	}
	got := make([]string, 0, len(objects))
	for i, o := range objects {
		got = append(got, o.Key)
		if o.Metadata["etcd-revision"] == "" || o.Size != int64(len(o.Key)) {
			t.Errorf("object %d: %+v", i, o)
		}
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("listed %d objects, want %d", len(got), len(want))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.lists < 2 {
		t.Errorf("%d listing pages for %d objects", f.lists, n+1)
	}
}

func TestListFailsWhenTheStoreRefusesIt(t *testing.T) {
	t.Parallel()
	_, srv := newFakeS3(t, defaultBackupRegion)
	store, err := OpenTarget(&BackupTarget{Name: "offsite", Endpoint: srv.URL, Bucket: "someone-elses", CACertificate: certificatePEM(srv)},
		map[string][]byte{backupAccessKeyID: []byte(fakeS3ID), backupSecretAccessKey: []byte(fakeS3Secret)})
	if err != nil {
		t.Fatal(err)
	}
	if objects, err := store.List(context.Background(), "web/"); err == nil || objects != nil {
		t.Errorf("listed %v, %v from a bucket that is not there", objects, err)
	}
}

func TestStatAndGetOfAMissingObjectAreErrNoObject(t *testing.T) {
	t.Parallel()
	_, srv := newFakeS3(t, defaultBackupRegion)
	store := openFake(t, srv, "")
	if _, err := store.Stat(context.Background(), "web/etcd/gone"); !errors.Is(err, ErrNoObject) {
		t.Errorf("stat: %v", err)
	}
	if r, err := store.Get(context.Background(), "web/etcd/gone"); !errors.Is(err, ErrNoObject) || r != nil {
		t.Errorf("get: %v", err)
	}
}

// Only a missing object is ErrNoObject: callers take that as "nothing there" and move on.
func TestARefusedReadIsNotErrNoObject(t *testing.T) {
	t.Parallel()
	f, srv := newFakeS3(t, defaultBackupRegion)
	f.put("snap", []byte("sealed"), nil)
	store, err := OpenTarget(&BackupTarget{Name: "offsite", Endpoint: srv.URL, Bucket: fakeS3Bucket, CACertificate: certificatePEM(srv)},
		map[string][]byte{backupAccessKeyID: []byte(fakeS3ID), backupSecretAccessKey: []byte("a revoked secret")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stat(context.Background(), "snap"); err == nil || errors.Is(err, ErrNoObject) {
		t.Errorf("stat with a wrong secret: %v", err)
	}
	if _, err := store.Get(context.Background(), "snap"); err == nil || errors.Is(err, ErrNoObject) {
		t.Errorf("get with a wrong secret: %v", err)
	}
}

func TestRemoveDeletesAndTakesAGoneObjectAsRemoved(t *testing.T) {
	t.Parallel()
	f, srv := newFakeS3(t, defaultBackupRegion)
	f.put("snap", []byte("sealed"), nil)
	f.put("keep", []byte("sealed"), nil)
	store := openFake(t, srv, "")
	ctx := context.Background()
	if err := store.Remove(ctx, "snap"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Stat(ctx, "snap"); !errors.Is(err, ErrNoObject) {
		t.Errorf("after removing: %v", err)
	}
	if _, err := store.Stat(ctx, "keep"); err != nil {
		t.Errorf("another object: %v", err)
	}
	if err := store.Remove(ctx, "snap"); err != nil {
		t.Errorf("removing it again: %v", err)
	}
}

// A restore hands the link to a node, which fetches it with nothing but the link.
func TestPresignedLinkFetchesTheObjectWithoutCredentials(t *testing.T) {
	t.Parallel()
	f, srv := newFakeS3(t, defaultBackupRegion)
	f.put("web/restore/token", []byte("sealed snapshot"), nil)
	link, err := openFake(t, srv, "").PresignGet(context.Background(), "web/restore/token", 5*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	if q := u.Query(); u.Scheme != "https" || u.Host != strings.TrimPrefix(srv.URL, "https://") || q.Get("X-Amz-Expires") != "300" {
		t.Errorf("link %s", link)
	}
	fetch := func(link string) (int, string) {
		t.Helper()
		req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, link, http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}
	if status, body := fetch(link); status != http.StatusOK || body != "sealed snapshot" {
		t.Errorf("fetching the link: %d %q", status, body)
	}
	// The signature covers the key: the link opens no other object.
	f.put("web/etcd/other", []byte("another"), nil)
	if status, _ := fetch(strings.Replace(link, "restore/token", "etcd/other", 1)); status != http.StatusForbidden {
		t.Errorf("a link changed to another key fetched it: %d", status)
	}
}

func TestStoreRefusesACertificateNotSignedByTheTargetsCA(t *testing.T) {
	t.Parallel()
	f, srv := newFakeS3(t, defaultBackupRegion)
	f.put("snap", []byte("sealed"), nil)
	// Every httptest server has the same certificate, so the other CA is made here.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "another CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	otherCA := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	credential := map[string][]byte{backupAccessKeyID: []byte(fakeS3ID), backupSecretAccessKey: []byte(fakeS3Secret)}
	for name, ca := range map[string]string{"no CA": "", "another CA": otherCA} {
		// minio-go retries a refused certificate ten times, which takes seconds a case.
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store, err := OpenTarget(&BackupTarget{Name: "offsite", Endpoint: srv.URL, Bucket: fakeS3Bucket, CACertificate: ca}, credential)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Stat(context.Background(), "snap"); err == nil || !strings.Contains(err.Error(), "certificate") {
				t.Errorf("got %v", err)
			}
		})
	}
}
