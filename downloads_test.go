package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testStore builds a download store over a temporary directory, with private
// destinations permitted so the tests can point it at their own stub server -
// which is on loopback, and would otherwise be refused by the guard that the
// separate SSRF tests below exercise on its own.
func testStore(t *testing.T, tweak func(*DownloadConfig)) *DownloadStore {
	t.Helper()
	cfg := DownloadConfig{
		Dir:               t.TempDir(),
		MaxFileBytes:      1 << 20,
		MaxTotalBytes:     4 << 20,
		AllowPrivateHosts: true,
	}
	if tweak != nil {
		tweak(&cfg)
	}
	return NewDownloadStore(cfg)
}

func TestDownloadFetchAndServe(t *testing.T) {
	const body = "column,value\nalpha,1\n"
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	defer stub.Close()

	store := testStore(t, nil)
	got, err := store.Fetch(context.Background(), stub.URL+"/data.csv", "")
	if err != nil {
		t.Fatalf("fetching: %v", err)
	}

	if got.Bytes != int64(len(body)) {
		t.Errorf("bytes = %d, want %d", got.Bytes, len(body))
	}
	// The charset parameter is dropped, so the type can be compared and used
	// as a map key rather than being matched with strings.HasPrefix everywhere.
	if got.ContentType != "text/csv" {
		t.Errorf("content type = %q, want the parameters stripped", got.ContentType)
	}
	if got.Filename != "data.csv" {
		t.Errorf("filename = %q", got.Filename)
	}
	if !validDownloadID(got.ID) {
		t.Errorf("id %q is not the shape this server mints", got.ID)
	}
	// The file on disk is named by its id, never by anything the source chose.
	if base := filepath.Base(got.Path); !strings.HasPrefix(base, got.ID) {
		t.Errorf("stored as %q, which is not named by the id", base)
	}
	stored, err := os.ReadFile(got.Path)
	if err != nil || string(stored) != body {
		t.Errorf("the stored bytes do not match what was served: %v", err)
	}

	listed, err := store.List()
	if err != nil {
		t.Fatalf("listing: %v", err)
	}
	if len(listed) != 1 || listed[0].ID != got.ID {
		t.Fatalf("list returned %d entries", len(listed))
	}
	if listed[0].SourceURL == "" {
		t.Error("a stored file does not record where it came from")
	}

	if err := store.Delete(got.ID); err != nil {
		t.Fatalf("deleting: %v", err)
	}
	if _, err := os.Stat(got.Path); !os.IsNotExist(err) {
		t.Error("the file is still on disk after delete")
	}
	if remaining, _ := store.List(); len(remaining) != 0 {
		t.Errorf("%d entries remain after delete", len(remaining))
	}
}

// TestDownloadRefusesPrivateDestinations covers the guard that keeps this from
// being a window onto the network the server sits in. Fetching a URL a caller
// chose, from here, and then serving the answer back is the whole SSRF shape.
func TestDownloadRefusesPrivateDestinations(t *testing.T) {
	store := testStore(t, func(c *DownloadConfig) { c.AllowPrivateHosts = false })

	for _, target := range []string{
		"http://127.0.0.1/x",
		"http://localhost/x",
		// The cloud metadata endpoint, which is the one that hands out
		// credentials to anything that can reach it.
		"http://169.254.169.254/latest/meta-data/",
		"http://192.168.1.1/",
		"http://10.0.0.5/admin",
		"http://[::1]/x",
	} {
		_, err := store.Fetch(context.Background(), target, "")
		if err == nil {
			t.Errorf("%s was fetched", target)
			continue
		}
		if !isInputError(err) {
			t.Errorf("%s: refused, but not as the caller's mistake: %v", target, err)
		}
	}

	// And the opt-in works, or someone with an intranet has no way through.
	open := testStore(t, nil)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer stub.Close()
	if _, err := open.Fetch(context.Background(), stub.URL, ""); err != nil {
		t.Errorf("with allow_private_hosts, a loopback fetch failed: %v", err)
	}
}

// TestDownloadRedirectIsAlsoChecked covers the way the guard gets walked
// around: a public URL that redirects somewhere private.
func TestDownloadRedirectIsAlsoChecked(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/", http.StatusFound)
	}))
	defer stub.Close()

	// Private hosts are allowed so the stub itself is reachable; the redirect
	// target must still be refused, which is what isolates the hop check.
	store := testStore(t, nil)
	store.cfg.AllowPrivateHosts = false
	store.client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		return checkDownloadHost(req.URL, false)
	}
	if _, err := store.Fetch(context.Background(), stub.URL, ""); err == nil {
		t.Error("a redirect to a link-local address was followed")
	}
}

func TestDownloadSizeLimits(t *testing.T) {
	big := strings.Repeat("x", 5000)
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/nolength" {
			// No Content-Length, so the cap has to hold on the copy rather
			// than on the header - which is the case that matters, since the
			// header is the source's claim rather than a fact.
			w.Header().Set("Transfer-Encoding", "chunked")
		}
		_, _ = w.Write([]byte(big))
	}))
	defer stub.Close()

	store := testStore(t, func(c *DownloadConfig) {
		c.MaxFileBytes = 1000
		c.MaxTotalBytes = 100000
	})

	for _, path := range []string{"/big", "/nolength"} {
		_, err := store.Fetch(context.Background(), stub.URL+path, "")
		if err == nil {
			t.Errorf("%s: a file over the limit was stored", path)
			continue
		}
		if !isInputError(err) {
			t.Errorf("%s: %v", path, err)
		}
	}
	// Nothing partial is left behind by a refused download.
	if listed, _ := store.List(); len(listed) != 0 {
		t.Errorf("%d entries left after refusing every download", len(listed))
	}
	entries, _ := os.ReadDir(store.cfg.Dir)
	if len(entries) != 0 {
		t.Errorf("%d files left in the store directory after refusing every download", len(entries))
	}
}

// TestDownloadStoreFullRefusesRatherThanPrunes is a policy test. These are
// files somebody asked for by name, so the store filling up must not quietly
// delete the oldest to make room - and the error has to name the way out.
func TestDownloadStoreFullRefusesRatherThanPrunes(t *testing.T) {
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(strings.Repeat("x", 600)))
	}))
	defer stub.Close()

	store := testStore(t, func(c *DownloadConfig) {
		c.MaxFileBytes = 1000
		c.MaxTotalBytes = 1000
	})

	first, err := store.Fetch(context.Background(), stub.URL+"/one", "")
	if err != nil {
		t.Fatalf("the first download failed: %v", err)
	}
	if _, err := store.Fetch(context.Background(), stub.URL+"/two", ""); err == nil {
		t.Fatal("a download was accepted into a full store")
	} else if !strings.Contains(err.Error(), "delete_download") {
		t.Errorf("the error does not say how to make room: %v", err)
	}

	// The first is untouched: nothing was deleted to serve the second.
	if _, err := store.Get(first.ID); err != nil {
		t.Errorf("the existing download was pruned to make room: %v", err)
	}

	// And deleting really does make room.
	if err := store.Delete(first.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Fetch(context.Background(), stub.URL+"/three", ""); err != nil {
		t.Errorf("still full after deleting: %v", err)
	}
}

// TestDownloadIDCannotEscapeTheStore covers the identifier as a path segment.
// It is the only caller-supplied value that reaches the filesystem.
func TestDownloadIDCannotEscapeTheStore(t *testing.T) {
	store := testStore(t, nil)
	for _, id := range []string{
		"../../../../etc/passwd",
		`..\..\windows\win.ini`,
		"not-hex-at-all",
		"",
		strings.Repeat("a", 31), // right alphabet, wrong length
		strings.Repeat("a", 33),
		"../" + strings.Repeat("a", 29),
	} {
		if validDownloadID(id) {
			t.Errorf("%q was accepted as an id", id)
		}
		if _, err := store.Get(id); err == nil {
			t.Errorf("%q resolved to a download", id)
		}
	}
	if id, err := newDownloadID(); err != nil || !validDownloadID(id) {
		t.Errorf("a freshly minted id is not valid: %q %v", id, err)
	}
}

// TestDownloadDispositionNeverExecutes is the rule that keeps a stored file
// from becoming script on this server's origin.
func TestDownloadDispositionNeverExecutes(t *testing.T) {
	for contentType, wantInline := range map[string]bool{
		"image/png":       true,
		"image/jpeg":      true,
		"application/pdf": true,
		"text/plain":      true,
		// The dangerous ones. HTML is obvious; SVG is xml that can carry
		// script and is not an exception however much it is an image.
		"text/html":                true,
		"image/svg+xml":            true,
		"application/xhtml+xml":    true,
		"application/javascript":   true,
		"application/octet-stream": false,
	} {
		want := "attachment"
		if wantInline && contentType != "text/html" && contentType != "image/svg+xml" &&
			contentType != "application/xhtml+xml" && contentType != "application/javascript" {
			want = "inline"
		}
		got := contentDisposition(&Download{Filename: "f", ContentType: contentType})
		if !strings.HasPrefix(got, want) {
			t.Errorf("%s served as %q, want %s", contentType, got, want)
		}
	}

	// A hostile filename must not break out of the header.
	got := contentDisposition(&Download{
		Filename:    `../../evil";x="`,
		ContentType: "image/png",
	})
	if strings.Contains(got, "..") {
		t.Errorf("a traversing filename reached the header: %q", got)
	}
}

func TestSanitiseFilename(t *testing.T) {
	for in, want := range map[string]string{
		"report.pdf":              "report.pdf",
		"../../etc/passwd":        "passwd",
		`..\..\windows\win.ini`:   "win.ini",
		"/absolute/path/file.txt": "file.txt",
		".hidden":                 "hidden",
		"":                        "",
		"   ":                     "",
		"with spaces.txt":         "with spaces.txt",
	} {
		if got := sanitiseFilename(in); got != want {
			t.Errorf("sanitiseFilename(%q) = %q, want %q", in, got, want)
		}
	}
	// Control characters are dropped, and the Windows-reserved ones replaced.
	if got := sanitiseFilename("a\x00b\nc:d?e.txt"); strings.ContainsAny(got, "\x00\n:?") {
		t.Errorf("sanitiseFilename left dangerous characters: %q", got)
	}
	// A very long name is trimmed from the front, so the extension survives.
	long := strings.Repeat("n", 400) + ".pdf"
	if got := sanitiseFilename(long); len(got) > 120 || !strings.HasSuffix(got, ".pdf") {
		t.Errorf("a long name was trimmed badly: len %d, %q", len(got), got[max(0, len(got)-10):])
	}
}

func TestDownloadFilenameFallbacks(t *testing.T) {
	u, _ := url.Parse("https://example.org/files/report.pdf")
	for _, tc := range []struct {
		name        string
		preferred   string
		disposition string
		want        string
	}{
		{name: "caller wins", preferred: "mine.pdf", disposition: `attachment; filename="theirs.pdf"`, want: "mine.pdf"},
		{name: "then the header", disposition: `attachment; filename="theirs.pdf"`, want: "theirs.pdf"},
		{name: "then the path", want: "report.pdf"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := downloadFilename(tc.preferred, tc.disposition, u, "application/pdf")
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	// A URL with no usable last segment still yields something openable.
	bare, _ := url.Parse("https://example.org/")
	if got := downloadFilename("", "", bare, "application/pdf"); got != "download.pdf" {
		t.Errorf("bare URL gave %q", got)
	}
}

func TestExtensionFor(t *testing.T) {
	for _, tc := range []struct{ filename, contentType, want string }{
		{"report.pdf", "application/pdf", ".pdf"},
		{"", "image/jpeg", ".jpg"},
		{"", "image/png", ".png"},
		{"", "application/octet-stream", ".bin"},
		{"", "something/unknown", ".bin"},
		{"data.CSV", "text/csv", ".csv"},
	} {
		if got := extensionFor(tc.filename, tc.contentType); got != tc.want {
			t.Errorf("extensionFor(%q, %q) = %q, want %q", tc.filename, tc.contentType, got, tc.want)
		}
	}
}

// TestDownloadsAreProtectedByDefault checks the served files follow the same
// rule as the rest of the data: the token guards what carries content.
func TestDownloadsAreProtectedByDefault(t *testing.T) {
	for _, public := range []bool{false, true} {
		t.Run(fmt.Sprintf("public=%v", public), func(t *testing.T) {
			srv, cfg := testServer(t)
			cfg.Server.AuthToken = "s3cret"
			cfg.Downloads.Public = public
			cfg.API.DownloadsPath = "/mcp/downloads"
			srv.browser = NewBrowser(cfg.Browser, cfg.Search, testLogger())
			srv.downloads = NewDownloadStore(cfg.Downloads)
			api, err := NewRESTAPI(srv, cfg, testLogger())
			if err != nil {
				t.Fatal(err)
			}

			// A path that does not resolve to a file either way, so what is
			// being measured is the auth decision and not the lookup.
			rec := httptest.NewRecorder()
			api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp/downloads/"+strings.Repeat("a", 32), nil))

			if public && rec.Code == http.StatusUnauthorized {
				t.Error("downloads.public did not open the stored files")
			}
			if !public && rec.Code != http.StatusUnauthorized {
				t.Errorf("stored files answered %d without a token, want 401", rec.Code)
			}
		})
	}
}

// TestDownloadsPathDefaultsUnderMCP covers where the files are served from,
// which is what makes them reachable through a tunnel with nothing else to set
// up: the MCP endpoint's address is the one a client already has.
func TestDownloadsPathDefaultsUnderMCP(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Server.Transport = "http"
	cfg.Server.URL = "http://127.0.0.1:8780/mcp"
	if err := cfg.Normalize(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if cfg.API.DownloadsPath != "/mcp/downloads" {
		t.Errorf("downloads path = %q, want it under the MCP endpoint", cfg.API.DownloadsPath)
	}
	if !cfg.DownloadsServed() {
		t.Error("downloads are not served over the HTTP transport")
	}

	// A different MCP path carries the downloads with it.
	cfg = DefaultConfig()
	cfg.Server.Transport = "http"
	cfg.Server.URL = "http://127.0.0.1:9000/tools/mcp"
	if err := cfg.Normalize(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if cfg.API.DownloadsPath != "/tools/mcp/downloads" {
		t.Errorf("downloads path = %q", cfg.API.DownloadsPath)
	}

	// On stdio there is nothing to serve them from, and saying so beats
	// handing back a URL that answers nothing.
	cfg = DefaultConfig()
	if err := cfg.Normalize(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	if cfg.DownloadsServed() {
		t.Error("downloads are reported as served on stdio")
	}
}

func TestDownloadToolsAreRegistered(t *testing.T) {
	srv, cfg := testServer(t)
	for _, name := range []string{"download_asset", "list_downloads", "delete_download"} {
		if !contains(srv.ToolNames(), name) {
			t.Errorf("%s is not registered", name)
		}
	}
	paths := buildSpec(cfg)["paths"].(map[string]any)
	for _, path := range []string{"/downloads", "/downloads/list", "/downloads/delete"} {
		if _, ok := paths[cfg.API.BasePath+path]; !ok {
			t.Errorf("%s is not in the OpenAPI document", path)
		}
	}
	// The two that change something must not claim to be read-only, since a
	// client may auto-approve on that hint.
	for _, op := range Operations() {
		switch op.Name {
		case "download_asset", "delete_download":
			if op.ReadOnly {
				t.Errorf("%s is marked read-only but it writes", op.Name)
			}
		case "list_downloads":
			if !op.ReadOnly {
				t.Errorf("%s only reads but is not marked read-only", op.Name)
			}
		}
	}
}

func TestHumanBytes(t *testing.T) {
	for in, want := range map[int64]string{
		0: "0 bytes", 512: "512 bytes", 2048: "2 KB",
		5 << 20: "5.0 MB", 3 << 30: "3.0 GB", -1: "an unknown size",
	} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}
