package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Downloads: fetching an asset once and then serving it back over HTTP.
//
// The searches return URLs - a patent PDF, an image, a document a result links
// to - and a URL is not always enough. The thing that wants the file may not be
// able to reach the site (a model with no network of its own), may not be able
// to authenticate to it, or may need a link that is still valid in ten minutes
// when the signed one is not. So the file is fetched once, kept, and served
// from this server under a stable address.
//
// That makes this server, for the files it has been asked to hold, a file host.
// Three things follow, and all three are the reason this file is longer than
// "fetch a URL and write it to disk":
//
//   - Fetching a URL chosen by a caller is a request made from wherever this
//     server sits. On a machine with a private network around it, that reaches
//     addresses the caller cannot, which is the classic way a scraper becomes a
//     window onto an internal network. Private destinations are refused.
//   - Anything stored is later served from this server's own origin. A stored
//     HTML file served inline is script running on that origin, against
//     whatever the tunnel URL is trusted for. Nothing is served in a way that
//     lets it execute.
//   - Disk is finite and callers are not careful, so both the file and the
//     store are bounded.

// downloadIDBytes is the length of a download identifier before hex encoding.
// Sixteen bytes is not for uniqueness - a counter would give that - but so the
// identifier cannot be guessed. It is the whole of the path, and an unguessable
// path is what keeps an anonymously served store from being enumerable.
const downloadIDBytes = 16

// Download is one stored asset.
type Download struct {
	ID string `json:"id"`

	// Filename is what to call it when saving, taken from the source and
	// sanitised. It is not where the file lives: that is ID, so a hostile or
	// merely awkward name cannot reach outside the store.
	Filename string `json:"filename"`

	// SourceURL is where it came from, so a stored file can always be traced
	// back to what produced it.
	SourceURL   string `json:"source_url"`
	ContentType string `json:"content_type"`
	Bytes       int64  `json:"bytes"`
	Downloaded  string `json:"downloaded_at"`

	// URL is where this server now serves it. Empty when the HTTP transport is
	// not running, since then there is nowhere to serve it from - the file is
	// still on disk, and Path says where.
	URL  string `json:"url,omitempty"`
	Path string `json:"path"`
}

// DownloadStore holds the fetched assets.
type DownloadStore struct {
	cfg    DownloadConfig
	client *http.Client
}

// NewDownloadStore prepares the store. The directory is created on first use
// rather than at startup: a server nobody asks to download anything should not
// leave a directory behind.
func NewDownloadStore(cfg DownloadConfig) *DownloadStore {
	return &DownloadStore{
		cfg: cfg,
		client: &http.Client{
			Timeout: 120 * time.Second,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 5 {
					return fmt.Errorf("too many redirects")
				}
				// Each hop is checked, not just the first. A redirect to a
				// private address is the ordinary way the destination guard
				// gets walked around.
				return checkDownloadHost(req.URL, cfg.AllowPrivateHosts)
			},
		},
	}
}

// Fetch downloads a URL into the store and returns what it stored.
func (s *DownloadStore) Fetch(ctx context.Context, rawURL, preferredName string) (*Download, error) {
	target, err := checkURL(rawURL)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(target)
	if err != nil {
		return nil, badInput("url %q: %v", rawURL, err)
	}
	if err := checkDownloadHost(parsed, s.cfg.AllowPrivateHosts); err != nil {
		return nil, err
	}
	limit, err := s.limitFor()
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, badInput("url %q: %v", rawURL, err)
	}
	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", target, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, badInput("%s: the server answered 404; the link may have expired", target)
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusUnauthorized:
		return nil, badInput("%s: the server refused the request (HTTP %d). Assets behind a "+
			"sign-in cannot be fetched this way", target, resp.StatusCode)
	case resp.StatusCode != http.StatusOK:
		return nil, fmt.Errorf("%s answered HTTP %d", hostOf(target), resp.StatusCode)
	}

	// The declared length is a hint worth acting on when it is present - there
	// is no reason to stream 400 MB only to refuse it - but it is not trusted,
	// so the copy below is bounded regardless.
	if resp.ContentLength > limit {
		return nil, s.tooLarge(target, resp.ContentLength, limit)
	}

	contentType := normaliseContentType(resp.Header.Get("Content-Type"))
	filename := downloadFilename(preferredName, resp.Header.Get("Content-Disposition"), parsed, contentType)

	id, err := newDownloadID()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(s.cfg.Dir, 0o755); err != nil {
		return nil, fmt.Errorf("creating the download directory: %w", err)
	}

	stored := filepath.Join(s.cfg.Dir, id+extensionFor(filename, contentType))
	file, err := os.Create(stored)
	if err != nil {
		return nil, fmt.Errorf("creating %s: %w", stored, err)
	}

	// One byte past the limit is read, so that a body which is exactly at it is
	// kept and one over is refused rather than silently truncated. A truncated
	// download that reports success is worse than a refused one: nothing
	// downstream can tell a half file from a whole one.
	written, copyErr := io.Copy(file, io.LimitReader(resp.Body, limit+1))
	closeErr := file.Close()
	switch {
	case copyErr != nil:
		os.Remove(stored) //nolint:errcheck
		return nil, fmt.Errorf("downloading %s: %w", target, copyErr)
	case closeErr != nil:
		os.Remove(stored) //nolint:errcheck
		return nil, fmt.Errorf("writing %s: %w", stored, closeErr)
	case written > limit:
		os.Remove(stored) //nolint:errcheck
		return nil, s.tooLarge(target, -1, limit)
	}

	download := &Download{
		ID:          id,
		Filename:    filename,
		SourceURL:   target,
		ContentType: contentType,
		Bytes:       written,
		Downloaded:  time.Now().UTC().Format(time.RFC3339),
		Path:        stored,
	}
	if err := s.writeMeta(download); err != nil {
		os.Remove(stored) //nolint:errcheck
		return nil, err
	}
	return download, nil
}

// List returns what is stored, newest first.
func (s *DownloadStore) List() ([]*Download, error) {
	entries, err := os.ReadDir(s.cfg.Dir)
	if err != nil {
		if os.IsNotExist(err) {
			// Nothing has been downloaded yet, which is not a failure.
			return nil, nil
		}
		return nil, fmt.Errorf("reading the download directory: %w", err)
	}
	var out []*Download
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), metaSuffix) {
			continue
		}
		download, err := s.readMeta(strings.TrimSuffix(entry.Name(), metaSuffix))
		if err != nil {
			continue // a half-written or hand-edited sidecar is skipped, not fatal
		}
		out = append(out, download)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Downloaded > out[j].Downloaded })
	return out, nil
}

// Get returns one stored asset by id.
func (s *DownloadStore) Get(id string) (*Download, error) {
	if !validDownloadID(id) {
		// Rejected on shape before it reaches the filesystem: the id is used to
		// build a path, and "../../etc/passwd" is a perfectly good string.
		return nil, badInput("%q is not a download id", id)
	}
	download, err := s.readMeta(id)
	if err != nil {
		return nil, badInput("no download %s; list_downloads shows what is stored", id)
	}
	return download, nil
}

// Delete removes a stored asset and its metadata.
func (s *DownloadStore) Delete(id string) error {
	download, err := s.Get(id)
	if err != nil {
		return err
	}
	if err := os.Remove(download.Path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", download.Path, err)
	}
	if err := os.Remove(s.metaPath(id)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing the metadata for %s: %w", id, err)
	}
	return nil
}

// Usage is the total size of the store.
func (s *DownloadStore) Usage() (int64, int, error) {
	downloads, err := s.List()
	if err != nil {
		return 0, 0, err
	}
	var total int64
	for _, d := range downloads {
		total += d.Bytes
	}
	return total, len(downloads), nil
}

// limitFor is how many bytes this download may be: the per-file cap, or
// whatever room is left in the store, whichever is smaller.
//
// Expressing the store limit as a bound on this one copy is what makes it
// exact. Checking "is the store already over its limit" before starting would
// let every download overshoot by its own size, and checking afterwards would
// mean writing a file in order to discover it was not allowed.
//
// Full means refused rather than pruned. These are files a caller asked for by
// name and may be about to use; deleting the oldest to make space for the
// newest would be a server quietly throwing away someone's data to serve a
// request it could have declined. delete_download is named in the error so the
// caller knows what to do about it.
func (s *DownloadStore) limitFor() (int64, error) {
	limit := s.cfg.MaxFileBytes
	if s.cfg.MaxTotalBytes <= 0 {
		return limit, nil
	}
	total, count, err := s.Usage()
	if err != nil {
		return 0, err
	}
	remaining := s.cfg.MaxTotalBytes - total
	if remaining <= 0 {
		return 0, badInput("the download store is full: %s across %d files, and the limit is %s. "+
			"Nothing is deleted automatically - use delete_download to make room, or raise "+
			"downloads.max_total_bytes", humanBytes(total), count, humanBytes(s.cfg.MaxTotalBytes))
	}
	return min(limit, remaining), nil
}

// tooLarge explains a refusal, distinguishing a file that is simply too big
// from one that would not fit in what is left. The two have different remedies
// and the same symptom.
func (s *DownloadStore) tooLarge(target string, size, limit int64) error {
	what := "is larger than"
	if size >= 0 {
		what = fmt.Sprintf("is %s, over", humanBytes(size))
	}
	if limit < s.cfg.MaxFileBytes {
		return badInput("%s %s the %s left in the download store. Use delete_download to make "+
			"room, or raise downloads.max_total_bytes", target, what, humanBytes(limit))
	}
	return badInput("%s %s the %s limit for a single download (downloads.max_file_bytes)",
		target, what, humanBytes(limit))
}

// metaSuffix names the sidecar that records where a file came from.
//
// A sidecar per file rather than one index: an index is a single thing to
// corrupt, needs locking across concurrent downloads, and can disagree with
// what is actually on disk. A file and its sidecar are written together and
// deleted together, and the store is whatever is there.
const metaSuffix = ".meta.json"

func (s *DownloadStore) metaPath(id string) string {
	return filepath.Join(s.cfg.Dir, id+metaSuffix)
}

func (s *DownloadStore) writeMeta(d *Download) error {
	encoded, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding the metadata for %s: %w", d.ID, err)
	}
	if err := os.WriteFile(s.metaPath(d.ID), encoded, 0o644); err != nil {
		return fmt.Errorf("writing the metadata for %s: %w", d.ID, err)
	}
	return nil
}

func (s *DownloadStore) readMeta(id string) (*Download, error) {
	if !validDownloadID(id) {
		return nil, fmt.Errorf("%q is not a download id", id)
	}
	raw, err := os.ReadFile(s.metaPath(id))
	if err != nil {
		return nil, err
	}
	var download Download
	if err := json.Unmarshal(raw, &download); err != nil {
		return nil, err
	}
	// The path is rebuilt from the id rather than trusted from the file, so a
	// hand-edited sidecar cannot point the server at something outside the
	// store.
	download.Path = filepath.Join(s.cfg.Dir, filepath.Base(download.Path))
	download.ID = id
	return &download, nil
}

// newDownloadID mints an unguessable identifier.
func newDownloadID() (string, error) {
	var buf [downloadIDBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", fmt.Errorf("generating a download id: %w", err)
	}
	return hex.EncodeToString(buf[:]), nil
}

// validDownloadID reports whether an id is one this server minted: hex, of the
// right length, and so incapable of naming anything but a file in the store.
func validDownloadID(id string) bool {
	if len(id) != downloadIDBytes*2 {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}

// checkDownloadHost refuses a destination this server should not be reaching
// on a caller's behalf.
//
// Fetching a URL somebody else chose means making a request from where this
// server is, which on any machine with a private network around it reaches
// addresses the caller cannot: another service on localhost, a database on the
// LAN, a cloud provider's metadata endpoint at 169.254.169.254 that hands out
// credentials to anything that asks. Storing the response and serving it back
// completes the loop and turns that reach into a readable answer.
//
// So private destinations are refused unless the operator says otherwise,
// which they might reasonably do to fetch from their own intranet.
func checkDownloadHost(u *url.URL, allowPrivate bool) error {
	if allowPrivate {
		return nil
	}
	host := u.Hostname()
	if host == "" {
		return badInput("url %q has no host", u.String())
	}

	// Resolved rather than pattern-matched, because "localtest.me" and any
	// name an attacker controls can point at 127.0.0.1 while looking public.
	addrs, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("looking up %s: %w", host, err)
	}
	for _, ip := range addrs {
		if isPrivateIP(ip) {
			return badInput("refusing to download from %s: it resolves to %s, which is a private "+
				"or local address. This server would be reaching somewhere the caller cannot, and "+
				"then serving the result back. Set downloads.allow_private_hosts to permit it",
				host, ip)
		}
	}
	return nil
}

// isPrivateIP reports whether an address is one that should not be reached on
// a caller's behalf.
func isPrivateIP(ip net.IP) bool {
	return ip.IsLoopback() ||
		ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() ||
		ip.IsUnspecified()
}

// normaliseContentType strips the parameters from a Content-Type and falls
// back to a neutral type when the server sent something unusable.
func normaliseContentType(raw string) string {
	parsed, _, err := mime.ParseMediaType(raw)
	if err != nil || parsed == "" {
		return "application/octet-stream"
	}
	return strings.ToLower(parsed)
}

// downloadFilename works out what to call the file, preferring what the caller
// asked for, then what the server said, then the last path segment.
func downloadFilename(preferred, disposition string, u *url.URL, contentType string) string {
	if name := sanitiseFilename(preferred); name != "" {
		return name
	}
	if disposition != "" {
		if _, params, err := mime.ParseMediaType(disposition); err == nil {
			if name := sanitiseFilename(params["filename"]); name != "" {
				return name
			}
		}
	}
	if name := sanitiseFilename(path.Base(u.Path)); name != "" && name != "/" && name != "." {
		return name
	}
	return "download" + extensionFor("", contentType)
}

// sanitiseFilename reduces a name to something safe to hand back as a
// suggestion. It is only ever a label - the file on disk is named by its id -
// but it reaches a caller that may write it somewhere, so the directory
// separators and the leading dots come out.
func sanitiseFilename(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	// Both separators, whatever platform the name came from.
	name = name[strings.LastIndexAny(name, `/\`)+1:]
	name = strings.TrimLeft(name, ".")
	name = strings.Map(func(r rune) rune {
		switch {
		case r < 0x20 || r == 0x7f: // control characters
			return -1
		case strings.ContainsRune(`<>:"|?*`, r): // refused by Windows
			return '_'
		}
		return r
	}, name)
	if len(name) > 120 {
		// Trimmed from the front so the extension survives, since that is the
		// part anything opening the file will look at.
		name = name[len(name)-120:]
	}
	return name
}

// extensionFor picks the extension the stored file is given, so that a browser
// fetching it has something to go on beyond the declared type.
func extensionFor(filename, contentType string) string {
	if ext := filepath.Ext(filename); ext != "" && len(ext) <= 8 {
		return strings.ToLower(ext)
	}
	// Deliberately a short table rather than mime.ExtensionsByType, which is
	// seeded from the system's mime.types and so gives a different answer on
	// different machines - and sometimes a startling one, like .jpe for JPEG.
	switch contentType {
	case "image/jpeg":
		return ".jpg"
	case "image/png":
		return ".png"
	case "image/gif":
		return ".gif"
	case "image/webp":
		return ".webp"
	case "image/svg+xml":
		return ".svg"
	case "application/pdf":
		return ".pdf"
	case "text/plain":
		return ".txt"
	case "text/csv":
		return ".csv"
	case "application/json":
		return ".json"
	case "application/zip":
		return ".zip"
	}
	return ".bin"
}

// humanBytes renders a size the way an error message wants it.
func humanBytes(n int64) string {
	switch {
	case n < 0:
		return "an unknown size"
	case n < 1024:
		return fmt.Sprintf("%d bytes", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.0f KB", float64(n)/1024)
	case n < 1024*1024*1024:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
	return fmt.Sprintf("%.1f GB", float64(n)/(1024*1024*1024))
}
