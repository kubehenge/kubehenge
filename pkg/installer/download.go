package installer

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Pins of what the installer downloads besides the release itself. Proxmox serves templates over
// plain HTTP, so the checksum is what vouches for the template. Once the mirror drops this point
// release, the index's signature vouches for the one it lists instead.
const (
	Template       = "debian-13-standard_13.6-1_amd64.tar.zst"
	TemplateSHA512 = "4c0c27ca6ceab5ef0b84db57825a00f26157ef1854bafe97297813e1cbe8ecb8cc9c453cab6b3b0efe1ba193a50c47ece1e41d950e411b8730b835b71e9e754b"
	TemplateMirror = "http://download.proxmox.com/images" //nolint:revive // Proxmox serves its mirror over HTTP, and the signed index and the pinned SHA-512 carry the integrity.
	templateKeys   = "/usr/share/doc/pve-manager/trustedkeys.gpg"
)

// Fetcher downloads files and checks them against a pinned checksum.
type Fetcher struct {
	Client *http.Client
	// Retries is the number of tries of a transfer, GitHub's release address aside.
	Retries int
	// Sleep waits between tries.
	Sleep func(time.Duration)
	// GitHubAPI is the base of GitHub's API, which serves a release asset when the download address fails.
	GitHubAPI string
	Token     string
	// releasesDown remembers that GitHub's download address failed, so every later asset of the run
	// goes through the API.
	releasesDown bool
}

// NewFetcher returns a Fetcher that goes through the proxy of Proxmox VE's datacenter options
// unless the environment names one.
func NewFetcher(proxy string) *Fetcher {
	transport := &http.Transport{}
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		transport = base.Clone()
	}
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	if proxy != "" {
		if u, err := url.Parse(proxy); err == nil {
			transport.Proxy = http.ProxyURL(u)
		}
	}
	return &Fetcher{
		Client: &http.Client{Transport: transport}, Retries: fetchRetries, Sleep: time.Sleep,
		GitHubAPI: "https://api.github.com", Token: os.Getenv("GITHUB_TOKEN"),
	}
}

// DatacenterProxy reads the HTTP proxy of Proxmox VE's datacenter options, empty when the
// environment names one or none is set.
func DatacenterProxy(data []byte) string {
	for _, k := range []string{"https_proxy", "HTTPS_PROXY", "http_proxy"} {
		if os.Getenv(k) != "" {
			return ""
		}
	}
	m := regexp.MustCompile(`(?m)^http_proxy:\s*(.*\S)\s*$`).FindSubmatch(data)
	if m == nil {
		return ""
	}
	return string(m[1])
}

// ProxyLabel is scheme://host:port of a proxy, without credentials or path, as safe to print.
func ProxyLabel(raw string) string {
	scheme, rest := "http", raw
	if before, after, ok := strings.Cut(raw, "://"); ok {
		scheme, rest = before, after
	}
	if i := strings.LastIndex(rest, "@"); i >= 0 {
		rest = rest[i+1:]
	}
	if i := strings.Index(rest, "/"); i >= 0 {
		rest = rest[:i]
	}
	return scheme + "://" + rest
}

// Scheme says which addresses a download accepts.
type Scheme int

// The schemes a download accepts.
const (
	// HTTPSOnly accepts https addresses.
	HTTPSOnly Scheme = iota
	// HTTPOrHTTPS also accepts http, for a download whose checksum comes from a signed source.
	HTTPOrHTTPS
)

// Fetch downloads from into dest and checks it against the SHA-256 or SHA-512 sum.
func (f *Fetcher) Fetch(ctx context.Context, from, dest, sum string, scheme Scheme, progress func(done, total int64)) error {
	if !strings.HasPrefix(from, "https://") && (scheme != HTTPOrHTTPS || !strings.HasPrefix(from, "http://")) {
		return fmt.Errorf("%s is not an https address", from)
	}
	github := strings.HasPrefix(from, "https://github.com/") && strings.Contains(from, "/releases/download/")
	if err := f.download(ctx, from, dest, github, progress); err != nil {
		return userErr("could not download "+from+" ("+err.Error()+")", "Check that this node reaches the address, or its datacenter HTTP proxy")
	}
	return checkSum(dest, sum, from)
}

// download fetches the file. A GitHub release asset goes through the API once the download
// address has failed, which then stays the way for the rest of the run.
func (f *Fetcher) download(ctx context.Context, from, dest string, github bool, progress func(done, total int64)) error {
	if github && f.releasesDown {
		return f.viaAPI(ctx, from, dest, progress)
	}
	tries := f.Retries
	if github {
		tries = 2
	}
	err := f.try(ctx, from, dest, tries, progress)
	if err == nil || !github {
		return err
	}
	f.releasesDown = true
	if apiErr := f.viaAPI(ctx, from, dest, progress); apiErr != nil {
		return fmt.Errorf("%w, nor through GitHub's API: %w", err, apiErr)
	}
	return nil
}

type countingReader struct {
	r        io.Reader
	done     int64
	total    int64
	progress func(done, total int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.done += int64(n)
	if c.progress != nil {
		c.progress(c.done, c.total)
	}
	return n, err //nolint:wrapcheck // io.Reader callers compare the error with io.EOF.
}

func checkSum(file, want, label string) error {
	h := sha256.New()
	if len(want) == sha512Hex {
		h = sha512.New()
	}
	in, err := os.Open(file) //nolint:gosec // the path is in the installer's own work directory.
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer func() { _ = in.Close() }()
	if _, err := io.Copy(h, in); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	if !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), want) {
		return userErr(label+" does not match its checksum", "Run the installer again, and report it if it fails again")
	}
	return nil
}

// TemplateGone reports whether Proxmox's mirror answers 404 for the pinned template. Any other
// answer leaves the pin to the download and its own error.
func (f *Fetcher) TemplateGone(ctx context.Context, from string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, from, http.NoBody)
	if err != nil {
		return false
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode == http.StatusNotFound
}

// TemplateFromIndex returns the newest amd64 Debian 13 standard template in Proxmox's template
// index as file, SHA-512 and location. The index is checked as pveam checks it, against this
// node's keyring of Proxmox's signing keys.
func (f *Fetcher) TemplateFromIndex(ctx context.Context, exec Exec, dir, mirror string) (IndexedTemplate, error) {
	index := dir + "/aplinfo-pve-9.dat"
	for _, suffix := range []string{".gz", ".asc"} {
		if err := f.try(ctx, mirror+"/aplinfo-pve-9.dat"+suffix, index+suffix, f.Retries, nil); err != nil {
			return IndexedTemplate{}, userErr("could not download Proxmox's template index "+mirror+"/aplinfo-pve-9.dat"+suffix, "Check that this node reaches the mirror")
		}
	}
	gz, err := os.Open(index + ".gz") //nolint:gosec // the path is in the installer's own work directory.
	if err != nil {
		return IndexedTemplate{}, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = gz.Close() }()
	zr, err := gzip.NewReader(gz)
	if err != nil {
		return IndexedTemplate{}, userErr("Proxmox's template index does not unpack", "")
	}
	data, err := io.ReadAll(zr)
	if err != nil || os.WriteFile(index, data, permFile) != nil {
		return IndexedTemplate{}, userErr("Proxmox's template index does not unpack", "")
	}
	if _, err := exec.Run(ctx, "sqv", "--keyring", templateKeys, index+".asc", index); err != nil {
		return IndexedTemplate{}, userErr("Proxmox's template index fails its signature check against "+templateKeys, "")
	}
	type entry struct{ ver, loc, sum string }
	var best []entry
	for block := range strings.SplitSeq(string(data), "\n\n") {
		var pkg, arch string
		var e entry
		for line := range strings.SplitSeq(block, "\n") {
			k, v, _ := strings.Cut(line, ":")
			v = strings.TrimSpace(v)
			switch k {
			case "Package":
				pkg = v
			case "Architecture":
				arch = v
			case "Version":
				e.ver = v
			case "Location":
				e.loc = v
			case "sha512sum":
				e.sum = v
			}
		}
		if pkg == "debian-13-standard" && arch == "amd64" && indexLocationRE.MatchString(e.loc) && indexSumRE.MatchString(e.sum) {
			best = append(best, e)
		}
	}
	if len(best) == 0 {
		return IndexedTemplate{}, userErr("Proxmox's template index lists no amd64 Debian 13 standard template", "")
	}
	slices.SortStableFunc(best, func(a, b entry) int {
		switch {
		case versionLess(a.ver, b.ver):
			return -1
		case versionLess(b.ver, a.ver):
			return 1
		default:
			return 0
		}
	})
	top := best[len(best)-1]
	return IndexedTemplate{File: top.loc[strings.LastIndex(top.loc, "/")+1:], SHA512: top.sum, Location: top.loc}, nil
}

// IndexedTemplate is a template as Proxmox's index lists it.
type IndexedTemplate struct {
	File, SHA512, Location string
}

var (
	indexLocationRE = regexp.MustCompile(`^system/[A-Za-z0-9._+-]+$`)
	indexSumRE      = regexp.MustCompile(`^[0-9a-f]{128}$`)
	digitsRE        = regexp.MustCompile(`\d+`)
)

func (f *Fetcher) try(ctx context.Context, from, dest string, tries int, progress func(done, total int64)) error {
	var err error
	for i := range tries {
		if i > 0 {
			f.Sleep(time.Duration(i) * time.Second)
		}
		if err = f.get(ctx, from, dest, nil, progress); err == nil || ctx.Err() != nil {
			return err
		}
	}
	return err
}

func (f *Fetcher) get(ctx context.Context, from, dest string, header map[string]string, progress func(done, total int64)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, from, http.NoBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return fmt.Errorf("send request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP status %d", resp.StatusCode)
	}
	out, err := os.Create(dest) //nolint:gosec // the path is in the installer's own work directory.
	if err != nil {
		return fmt.Errorf("create: %w", err)
	}
	defer func() { _ = out.Close() }()
	if _, err = io.Copy(out, &countingReader{r: resp.Body, total: resp.ContentLength, progress: progress}); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return nil
}

// viaAPI fetches a GitHub release asset through the API, by its tag and name.
func (f *Fetcher) viaAPI(ctx context.Context, from, dest string, progress func(done, total int64)) error {
	rest := strings.TrimPrefix(from, "https://github.com/")
	repo, tagAndName, ok := strings.Cut(rest, "/releases/download/")
	tag, name, ok2 := strings.Cut(tagAndName, "/")
	if !ok || !ok2 {
		return fmt.Errorf("%s is no release asset", from)
	}
	header := map[string]string{}
	if f.Token != "" {
		header["Authorization"] = "Bearer " + f.Token
	}
	meta := dest + ".release"
	defer func() { _ = os.Remove(meta) }()
	if err := f.get(ctx, f.GitHubAPI+"/repos/"+repo+"/releases/tags/"+tag, meta, header, nil); err != nil {
		return err
	}
	data, err := os.ReadFile(meta) //nolint:gosec // the path is in the installer's own work directory.
	if err != nil {
		return fmt.Errorf("read file: %w", err)
	}
	var rel releaseAssets
	if err := json.Unmarshal(data, &rel); err != nil {
		return fmt.Errorf("unmarshal: %w", err)
	}
	for _, a := range rel.Assets {
		if a.Name == name {
			header["Accept"] = "application/octet-stream"
			return f.get(ctx, a.URL, dest, header, progress)
		}
	}
	return fmt.Errorf("release %s has no asset %s", tag, name)
}

type releaseAsset struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type releaseAssets struct {
	Assets []releaseAsset `json:"assets"`
}

// versionLess compares versions such as 13.6-1 by their numeric parts.
func versionLess(a, b string) bool {
	split := func(s string) []int {
		found := digitsRE.FindAllString(s, -1)
		parts := make([]int, 0, len(found))
		for _, p := range found {
			n, _ := strconv.Atoi(p)
			parts = append(parts, n)
		}
		return parts
	}
	pa, pb := split(a), split(b)
	for i := 0; i < len(pa) && i < len(pb); i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return len(pa) < len(pb)
}
