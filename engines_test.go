package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

func testLogger() *log.Logger { return log.New(io.Discard, "", 0) }

func TestEngineRegistry(t *testing.T) {
	for _, name := range []string{
		EngineGoogle, EngineDuckDuckGo, EngineBing, EngineBrave, EngineStartpage, EngineMojeek,
	} {
		engine, err := LookupEngine(name)
		if err != nil {
			t.Errorf("engine %q is not registered: %v", name, err)
			continue
		}
		if engine.Description() == "" {
			t.Errorf("%s: no description", name)
		}
	}
	if _, err := LookupEngine("altavista"); err == nil {
		t.Error("an unknown engine was accepted")
	}
	// The names are what the tool schema's enum offers, so they have to be
	// stable and sorted.
	names := EngineNames()
	for i := 1; i < len(names); i++ {
		if names[i-1] > names[i] {
			t.Fatalf("engine names are not sorted: %q before %q", names[i-1], names[i])
		}
	}
}

// TestUnwrapDecodesTrackers covers the two click wrappers whose destination is
// carried in the URL itself. Google's third shape is opaque and can only be
// resolved over the network, which TestUnwrapResolvesGoogleRedirect covers with
// a stub.
func TestUnwrapDecodesTrackers(t *testing.T) {
	s := NewScraper(nil, SearchConfig{})
	for _, tc := range []struct {
		name, in, want string
		unwrapped      bool
	}{
		{
			name:      "duckduckgo",
			in:        "https://duckduckgo.com/l/?uddg=https%3A%2F%2Fgithub.com%2Fmark3labs%2Fmcp%2Dgo&rut=abc",
			want:      "https://github.com/mark3labs/mcp-go",
			unwrapped: true,
		},
		{
			// "a1" prefix, then the destination as base64url.
			name:      "bing",
			in:        "https://www.bing.com/ck/a?!&&p=x&u=a1aHR0cHM6Ly9nby5kZXYv",
			want:      "https://go.dev/",
			unwrapped: true,
		},
		{
			name:      "google url form",
			in:        "https://www.google.com/url?q=https://example.org/page&sa=t",
			want:      "https://example.org/page",
			unwrapped: true,
		},
		{
			name:      "already a destination",
			in:        "https://example.org/page",
			want:      "https://example.org/page",
			unwrapped: false,
		},
		{
			name:      "not a url",
			in:        "not a url",
			want:      "not a url",
			unwrapped: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := s.Unwrap(context.Background(), tc.in)
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
			if ok != tc.unwrapped {
				t.Errorf("unwrapped=%v, want %v", ok, tc.unwrapped)
			}
		})
	}
}

// TestUnwrapResolvesGoogleRedirect points the resolver at a stub that behaves
// the way Google's /goto does - a 302 with the destination in Location - and
// checks that the redirect is read rather than followed. Following it would
// mean fetching every result page on every search.
func TestUnwrapResolvesGoogleRedirect(t *testing.T) {
	var fetched int
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/goto" {
			fetched++
			w.Header().Set("Location", "https://example.org/destination")
			w.WriteHeader(http.StatusFound)
			return
		}
		t.Errorf("the resolver followed the redirect to %s", r.URL.Path)
	}))
	defer stub.Close()

	s := NewScraper(nil, SearchConfig{})
	got, ok := s.resolve(context.Background(), stub.URL+"/goto?url=opaque")
	if !ok {
		t.Fatal("the redirect was not resolved")
	}
	if got != "https://example.org/destination" {
		t.Errorf("got %q", got)
	}
	if fetched != 1 {
		t.Errorf("the tracker was fetched %d times, want 1", fetched)
	}
}

func TestUnwrapRespectsTheSetting(t *testing.T) {
	// With resolution off, the opaque Google shape is left alone rather than
	// being fetched. The decodable shapes are still decoded: they cost nothing
	// and the setting is about network calls.
	s := NewScraper(nil, SearchConfig{ResolveRedirects: boolPtr(false)})
	in := "https://www.google.com/goto?url=opaque"
	if got, ok := s.Unwrap(context.Background(), in); ok || got != in {
		t.Errorf("got %q, unwrapped=%v; want the tracker left alone", got, ok)
	}
}

func TestIsTracker(t *testing.T) {
	for in, want := range map[string]bool{
		"https://www.google.com/goto?url=x": true,
		"https://www.google.com/url?q=x":    true,
		"https://www.bing.com/ck/a?u=a1x":   true,
		"https://duckduckgo.com/l/?uddg=x":  true,
		"https://github.com/modelcontext":   false,
		"https://www.google.com/search?q=x": false,
		"https://news.ycombinator.com/item": false,
	} {
		if got := isTracker(in); got != want {
			t.Errorf("isTracker(%q) = %v, want %v", in, got, want)
		}
	}
}

// TestSearchQueryFolding covers the parts of a query that are expressed in the
// query text rather than as engine parameters, because every engine here
// understands them and none of them spell the parameter the same way.
func TestSearchQueryFolding(t *testing.T) {
	q := SearchQuery{Query: "context cancellation", Site: "go.dev", NumResults: 5, Page: 3}
	if got, want := q.Text(), "site:go.dev context cancellation"; got != want {
		t.Errorf("Text() = %q, want %q", got, want)
	}
	if got, want := q.Offset(), 10; got != want {
		t.Errorf("Offset() = %d, want %d", got, want)
	}
	if got := (SearchQuery{Query: "x", Page: 1}).Offset(); got != 0 {
		t.Errorf("page 1 offset = %d, want 0", got)
	}
}

func TestSearchArgsValidation(t *testing.T) {
	cfg := SearchConfig{MaxResults: 20}
	for _, tc := range []struct {
		name    string
		args    searchArgs
		wantErr bool
		check   func(SearchQuery) string
	}{
		{
			name:    "empty query",
			args:    searchArgs{Query: "   "},
			wantErr: true,
		},
		{
			name:    "unknown time range",
			args:    searchArgs{Query: "x", TimeRange: "last tuesday"},
			wantErr: true,
		},
		{
			name:    "page beyond the useful range",
			args:    searchArgs{Query: "x", Page: 50},
			wantErr: true,
		},
		{
			name: "defaults applied",
			args: searchArgs{Query: " spaced "},
			check: func(q SearchQuery) string {
				if q.Query != "spaced" {
					return "the query was not trimmed"
				}
				if q.NumResults != 5 {
					return "num_results did not take its default"
				}
				if q.Page != 1 {
					return "page did not default to 1"
				}
				return ""
			},
		},
		{
			name: "num_results clamped to the ceiling",
			args: searchArgs{Query: "x", NumResults: 500},
			check: func(q SearchQuery) string {
				if q.NumResults != 20 {
					return "num_results was not clamped to max_results"
				}
				return ""
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := tc.args.query(cfg)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error")
				}
				if !isInputError(err) {
					t.Errorf("the error is not an InputError, so it would be reported as a server fault: %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.check != nil {
				if problem := tc.check(q); problem != "" {
					t.Error(problem)
				}
			}
		})
	}
}

// TestSafeSearchIsStickyFromConfig checks that a server configured for safe
// search cannot have it turned off by a tool call. The config is the
// operator's decision and a caller must not be able to override it downward.
func TestSafeSearchIsStickyFromConfig(t *testing.T) {
	q, err := searchArgs{Query: "x", SafeSearch: false}.query(SearchConfig{MaxResults: 10, SafeSearch: true})
	if err != nil {
		t.Fatal(err)
	}
	if !q.SafeSearch {
		t.Error("a call turned off the safe search the operator configured")
	}
}

func TestCheckURL(t *testing.T) {
	for _, tc := range []struct {
		in, want string
		wantErr  bool
	}{
		{in: "https://example.org/a", want: "https://example.org/a"},
		{in: "http://example.org", want: "http://example.org"},
		// A bare host is what a person types; upgrading it beats refusing it.
		{in: "example.org/docs", want: "https://example.org/docs"},
		{in: "  https://example.org  ", want: "https://example.org"},
		{in: "", wantErr: true},
		{in: "file:///etc/passwd", wantErr: true},
		{in: "ftp://example.org", wantErr: true},
	} {
		got, err := checkURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("checkURL(%q) was accepted", tc.in)
			} else if !isInputError(err) {
				t.Errorf("checkURL(%q): error is not an InputError", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("checkURL(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("checkURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDuckDuckGoParsing runs the DuckDuckGo engine against a stub serving the
// markup its HTML endpoint actually returns. It is the one engine whose whole
// path - fetch, parse, unwrap - can be exercised without a browser, so it is
// the one worth covering end to end.
func TestDuckDuckGoParsing(t *testing.T) {
	const page = `<html><body>
<div class="result results_links">
  <h2 class="result__title">
    <a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fgo.dev%2Fdoc%2F&amp;rut=x">The Go docs</a>
  </h2>
  <a class="result__url">go.dev/doc</a>
  <a class="result__snippet">Documentation for the Go programming language.</a>
</div>
<div class="result results_links">
  <h2 class="result__title">
    <a class="result__a" href="//duckduckgo.com/l/?uddg=https%3A%2F%2Fpkg.go.dev%2F">Go packages</a>
  </h2>
  <a class="result__snippet">Find packages.</a>
</div>
</body></html>`

	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("q") == "" {
			t.Error("the query was not sent")
		}
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(page))
	}))
	defer stub.Close()

	// The engine is exercised through its parsing rather than its URL, so the
	// body is fetched here and handed to the same parse the engine runs.
	s := NewScraper(nil, SearchConfig{})
	body, err := s.Get(context.Background(), stub.URL+"/?q=go+docs")
	if err != nil {
		t.Fatalf("fetching the stub: %v", err)
	}
	if !strings.Contains(body, "result__a") {
		t.Fatal("the stub did not serve the expected markup")
	}

	results := parseDuckDuckGo(body, 5)
	if len(results) != 2 {
		t.Fatalf("parsed %d results, want 2", len(results))
	}
	first := results[0]
	if first.Title != "The Go docs" {
		t.Errorf("title = %q", first.Title)
	}
	if first.Snippet != "Documentation for the Go programming language." {
		t.Errorf("snippet = %q", first.Snippet)
	}
	if first.DisplayURL != "go.dev/doc" {
		t.Errorf("display url = %q", first.DisplayURL)
	}
	// The href is protocol-relative in the markup and has to be absolute here.
	if !strings.HasPrefix(first.URL, "https://duckduckgo.com/l/") {
		t.Errorf("url = %q, want the absolute tracker before unwrapping", first.URL)
	}

	s.UnwrapAll(context.Background(), results)
	if results[0].URL != "https://go.dev/doc/" {
		t.Errorf("after unwrapping, url = %q", results[0].URL)
	}
	if results[0].Redirected {
		t.Error("a successfully unwrapped result is still marked as redirected")
	}
}

func TestPlausiblePrice(t *testing.T) {
	for in, want := range map[string]string{
		"$328.21":    "$328.21",
		"7,747.71":   "7,747.71",
		"328.21 USD": "328.21 USD",
		"€1.234,00":  "€1.234,00",
		// The odometer: every digit column, all in the DOM at once.
		"9\n8\n7\n6\n5\n4\n3\n2\n1\n0":  "",
		"":                              "",
		"Closed: Sep 3, 4:00:01 PM UTC": "",
		"n/a":                           "",
	} {
		if got := plausiblePrice(in); got != want {
			t.Errorf("plausiblePrice(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCleanTextAndTruncate(t *testing.T) {
	if got, want := cleanText("a\n\n\n\n\nb\n  \n\nc  \n"), "a\n\nb\n\nc"; got != want {
		t.Errorf("cleanText = %q, want %q", got, want)
	}
	long := strings.Repeat("x", 100)
	got := truncate(long, 20)
	if !strings.HasPrefix(got, strings.Repeat("x", 20)) {
		t.Error("truncate did not keep the first 20 characters")
	}
	if !strings.Contains(got, "truncated") {
		t.Error("truncate did not say it had truncated; a model cannot tell that from the end of a page")
	}
	if truncate("short", 20) != "short" {
		t.Error("truncate altered text that was already short enough")
	}
	// Cutting must not split a multi-byte character in half.
	if got := truncate(strings.Repeat("é", 10), 5); !utf8.ValidString(got) {
		t.Error("truncate produced invalid UTF-8")
	}
}

func TestLanguageCode(t *testing.T) {
	for in, want := range map[string]string{
		"Japanese":  "ja",
		"japanese":  "ja",
		"  German ": "de",
		"ja":        "ja",
		// An unrecognised name passes through: a caller who knows the code
		// should not have to be in this table.
		"klingon": "klingon",
	} {
		if got := languageCode(in); got != want {
			t.Errorf("languageCode(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestMojeekParsing is a regression test for the bug that made this engine
// report every search as finding nothing.
//
// The parser selected li.r. Mojeek gives each result its rank as a class -
// r1, r2, r3 - and class matching is by whole word, so it matched nothing on a
// page full of results. That is the exact failure mode the whole design is
// wary of: an extraction that breaks produces an empty list, which is
// indistinguishable from a query with no matches.
func TestMojeekParsing(t *testing.T) {
	fixture, err := os.ReadFile("testdata/mojeek_results.html")
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	body := string(fixture)

	if isMojeekChallenge(body) {
		t.Fatal("a results page was mistaken for the verification challenge")
	}

	results := parseMojeek(body, 10)
	if len(results) != 4 {
		t.Fatalf("parsed %d results, want 4", len(results))
	}

	first := results[0]
	if first.Title != "Specification - Model Context Protocol" {
		t.Errorf("title = %q", first.Title)
	}
	if first.URL != "https://modelcontextprotocol.io/specification/2025-06-18" {
		t.Errorf("url = %q", first.URL)
	}
	// Mojeek links straight to the destination, so nothing should look like a
	// tracker and nothing needs unwrapping.
	if isTracker(first.URL) {
		t.Errorf("a Mojeek result was taken for a redirect: %q", first.URL)
	}
	if !strings.Contains(first.Snippet, "open protocol that enables") {
		t.Errorf("snippet = %q", first.Snippet)
	}
	// The snippet is marked up with <strong> around the query terms; the text
	// has to come through as prose rather than with the markup in it.
	if strings.Contains(first.Snippet, "<") {
		t.Errorf("the snippet still carries markup: %q", first.Snippet)
	}
	if !strings.Contains(first.DisplayURL, "modelcontextprotocol.io") {
		t.Errorf("display url = %q", first.DisplayURL)
	}

	// Every rank class must be picked up, not just the first.
	for i, r := range results {
		if r.Title == "" || r.URL == "" {
			t.Errorf("result %d is incomplete: %+v", i, r)
		}
	}
	if results[2].Title != "MCP Go SDK" {
		t.Errorf("the third result was missed: %q", results[2].Title)
	}
	// A result with no snippet is still a result.
	if results[3].Title != "A result with no snippet" || results[3].Snippet != "" {
		t.Errorf("the snippetless result was mishandled: %+v", results[3])
	}

	// The limit is honoured.
	if got := len(parseMojeek(body, 2)); got != 2 {
		t.Errorf("asked for 2 results, got %d", got)
	}
}

// TestMojeekChallengeDetection covers telling the gate from a results page.
// Getting this wrong in the quiet direction is the worse one: a challenge page
// parsed as results reports "no matches" for a query that was never run.
func TestMojeekChallengeDetection(t *testing.T) {
	challenges := []string{
		`<html><head><title>Captcha</title></head><body>...</body></html>`,
		`<html><body><altcha-widget challengeurl="/x"></altcha-widget></body></html>`,
		`<html><body><h1>Verification required</h1></body></html>`,
		`<html><body><label>I'm not a robot</label></body></html>`,
	}
	for _, body := range challenges {
		if !isMojeekChallenge(body) {
			t.Errorf("a challenge page was not recognised: %.60s", body)
		}
		// And it must not be silently parsed into an empty result set.
		if got := parseMojeek(body, 10); len(got) != 0 {
			t.Errorf("a challenge page produced %d results", len(got))
		}
	}

	fixture, err := os.ReadFile("testdata/mojeek_results.html")
	if err != nil {
		t.Fatal(err)
	}
	if isMojeekChallenge(string(fixture)) {
		t.Error("a real results page was taken for a challenge")
	}
}

// TestMojeekChallengeIsARefusal checks the harness reports the gate as the
// service declining rather than as this server being broken - and that its
// message says what actually lifts it, since unlike every other refusal here
// this one does not clear by waiting.
func TestMojeekChallengeIsARefusal(t *testing.T) {
	if !isRefusal(ErrMojeekChallenge) {
		t.Error("the Mojeek challenge is classified as a fault in this server")
	}
	wrapped := fmt.Errorf("mojeek: %w", ErrMojeekChallenge)
	if !isRefusal(wrapped) {
		t.Error("the wrapped challenge is classified as a fault")
	}
	for _, want := range []string{"I'm not a robot", "--cdp", "duckduckgo"} {
		if !strings.Contains(ErrMojeekChallenge.Error(), want) {
			t.Errorf("the message does not mention %q, so a caller cannot act on it", want)
		}
	}
}

// TestMojeekParsingAlternateLayout covers the container class changing under
// the parser. Mojeek does not always serve ul.results-standard, and when it
// does not, the old parser found nothing and every search came back empty
// while the engine itself was answering normally. The shape of a result is
// what selects it now, so a renamed list is not a break.
func TestMojeekParsingAlternateLayout(t *testing.T) {
	body := `<html><body>
<nav><ul class="main-nav"><li><a href="/images">Images</a></li></ul></nav>
<ul class="results-web">
  <li class="r1"><h3><a href="https://example.com/one">First result</a></h3>
    <p class="s">A snippet for the first result.</p></li>
  <li class="r2"><h3><a href="/out?u=https://example.com/two">Second result</a></h3></li>
</ul>
<ul class="page-links"><li><a href="/search?q=x&amp;s=10">2</a></li></ul>
</body></html>`

	results := parseMojeek(body, 10)
	if len(results) != 2 {
		t.Fatalf("parsed %d results from the alternate layout, want 2: %+v", len(results), results)
	}
	if results[0].Title != "First result" || results[0].URL != "https://example.com/one" {
		t.Errorf("first result parsed as %+v", results[0])
	}
	if results[0].Snippet != "A snippet for the first result." {
		t.Errorf("snippet parsed as %q", results[0].Snippet)
	}
	if results[1].URL != "https://www.mojeek.com/out?u=https://example.com/two" {
		t.Errorf("relative href was not made absolute: %q", results[1].URL)
	}
}
