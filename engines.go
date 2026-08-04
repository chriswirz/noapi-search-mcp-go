package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// The engines web_search can be asked for. Google is the default because it is
// what the rest of this server scrapes and what people mean by "search"; the
// others exist because Google rate-limits scrapers and a search tool that
// stops working for twenty minutes is worse than one that quietly changes
// engine.
const (
	EngineGoogle     = "google"
	EngineDuckDuckGo = "duckduckgo"
	EngineBing       = "bing"
	EngineBrave      = "brave"
	EngineStartpage  = "startpage"
	EngineMojeek     = "mojeek"
)

// SearchQuery is one search, whichever engine answers it. Not every engine
// honours every field - Mojeek has no time filter, DuckDuckGo's regions are
// its own - and an engine that cannot express a constraint applies what it can
// and reports the rest in the result's Notes rather than pretending.
type SearchQuery struct {
	Query      string
	NumResults int
	Page       int
	Site       string
	TimeRange  string
	Language   string
	Region     string
	SafeSearch bool
}

// TimeRanges are the values TimeRange accepts.
var TimeRanges = []string{"past_hour", "past_day", "past_week", "past_month", "past_year"}

// googleTBS maps a time range onto Google's tbs parameter.
var googleTBS = map[string]string{
	"past_hour":  "qdr:h",
	"past_day":   "qdr:d",
	"past_week":  "qdr:w",
	"past_month": "qdr:m",
	"past_year":  "qdr:y",
}

// ddgTimeFilter maps a time range onto DuckDuckGo's df parameter.
var ddgTimeFilter = map[string]string{
	"past_day":   "d",
	"past_week":  "w",
	"past_month": "m",
	"past_year":  "y",
}

// bingFreshness maps a time range onto Bing's filters parameter.
var bingFreshness = map[string]string{
	"past_day":   "Day",
	"past_week":  "Week",
	"past_month": "Month",
}

// Text is the query as it goes to the engine, with a site: restriction folded
// in. Every engine here understands the site: operator, which is why it is
// expressed in the query rather than as a per-engine parameter.
func (q SearchQuery) Text() string {
	if q.Site == "" {
		return q.Query
	}
	return "site:" + q.Site + " " + q.Query
}

// Offset is how many results to skip, which is what a page number means.
func (q SearchQuery) Offset() int {
	if q.Page <= 1 {
		return 0
	}
	return (q.Page - 1) * q.NumResults
}

// SearchResult is one hit, in the shape every engine is normalised into.
type SearchResult struct {
	Rank int `json:"rank"`

	Title string `json:"title"`

	// URL is the destination, unwrapped from the engine's click tracker when
	// search.resolve_redirects is on. When it could not be unwrapped this is
	// the tracker URL itself and Redirected says so, rather than the caller
	// being handed a google.com link that looks like a real one.
	URL string `json:"url"`

	// DisplayURL is what the engine printed under the title. It survives when
	// the real URL cannot be recovered, and it is the honest thing to show a
	// person even when it can.
	DisplayURL string `json:"display_url,omitempty"`

	Snippet string `json:"snippet,omitempty"`

	// Redirected reports that URL is still the engine's tracker.
	Redirected bool `json:"redirected,omitempty"`
}

// SearchResponse is a whole answer: the results, and enough about how they
// were obtained that a surprising answer can be explained rather than guessed
// at.
type SearchResponse struct {
	Engine  string         `json:"engine"`
	Query   string         `json:"query"`
	Page    int            `json:"page,omitempty"`
	Count   int            `json:"count"`
	Results []SearchResult `json:"results"`

	// Notes carries what the engine could not do: a time filter it does not
	// support, a region it ignored. A constraint silently dropped is how a
	// caller comes to trust results that do not meet it.
	Notes []string `json:"notes,omitempty"`
}

// Engine is one search back end.
type Engine interface {
	// Name is the value web_search's engine argument takes.
	Name() string

	// Description is one line, for the tool schema and the OpenAPI spec.
	Description() string

	// NeedsBrowser reports whether Search drives the headless browser. An
	// engine that does not is usable on a machine with no Chromium installed,
	// which is worth knowing before the first call fails.
	NeedsBrowser() bool

	// Search runs one query. A nil error with no results means the engine
	// answered and found nothing, which is different from failing.
	Search(ctx context.Context, s *Scraper, q SearchQuery) (*SearchResponse, error)
}

// engines is the registry, built once at init.
var engines = map[string]Engine{}

func registerEngine(e Engine) { engines[e.Name()] = e }

func init() {
	registerEngine(googleEngine{})
	registerEngine(duckDuckGoEngine{})
	registerEngine(bingEngine{})
	registerEngine(braveEngine{})
	registerEngine(startpageEngine{})
	registerEngine(mojeekEngine{})
}

// IsEngine reports whether name is a known engine.
func IsEngine(name string) bool { _, ok := engines[name]; return ok }

// LookupEngine returns an engine by name.
func LookupEngine(name string) (Engine, error) {
	e, ok := engines[name]
	if !ok {
		return nil, fmt.Errorf("unknown engine %q; want one of %s", name, strings.Join(EngineNames(), ", "))
	}
	return e, nil
}

// EngineNames is every registered engine, sorted so the list is stable in help
// text, tool schemas and the OpenAPI document alike.
func EngineNames() []string {
	names := make([]string, 0, len(engines))
	for name := range engines {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// AllEngines returns the registered engines in name order.
func AllEngines() []Engine {
	out := make([]Engine, 0, len(engines))
	for _, name := range EngineNames() {
		out = append(out, engines[name])
	}
	return out
}

// Scraper is what an engine is handed: the browser, an HTTP client, and the
// settings that decide how far to go in cleaning results up.
type Scraper struct {
	Browser *Browser
	Search  SearchConfig
	HTTP    *http.Client

	// redirects is the client used to unwrap a tracker URL. It refuses to
	// follow, because the Location header of the first hop is the whole answer
	// and actually fetching the destination would mean loading every result
	// page - slow, and a visit the caller did not ask for.
	redirects *http.Client
}

// NewScraper builds the scraper the tools share.
func NewScraper(b *Browser, cfg SearchConfig) *Scraper {
	return &Scraper{
		Browser: b,
		Search:  cfg,
		HTTP: &http.Client{
			Timeout: 20 * time.Second,
		},
		redirects: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
}

// browserUserAgent is what the plain-HTTP engines present. It has to look like
// a browser or the HTML endpoints answer with a consent page or nothing at
// all, and it is the one place this server states a user agent by hand - the
// browser engines use whatever the launched binary actually reports.
const browserUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

// Get fetches a URL with browser-like headers and returns the body.
func (s *Scraper) Get(ctx context.Context, rawURL string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", browserUserAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close() //nolint:errcheck
	body, err := readAllLimited(resp.Body, 8<<20)
	if err != nil {
		return "", err
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("%w (HTTP %d from %s)", ErrBlocked, resp.StatusCode, hostOf(rawURL))
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s answered HTTP %d", hostOf(rawURL), resp.StatusCode)
	}
	return string(body), nil
}

func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return raw
}

// Unwrap turns an engine's click-tracker URL into the destination it points
// at. Two of the three shapes carry the target encoded in the URL and cost
// nothing to decode; Google's is opaque, and the only way to read it is to ask
// Google where it goes, which is what resolve does.
//
// A tracker that cannot be unwrapped is returned unchanged with ok false, so
// the caller can say so rather than presenting a search-engine link as if it
// were the result.
func (s *Scraper) Unwrap(ctx context.Context, raw string) (string, bool) {
	if raw == "" || !strings.HasPrefix(raw, "http") {
		return raw, false
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw, false
	}
	switch {
	// DuckDuckGo: /l/?uddg=<percent-encoded destination>
	case strings.HasSuffix(u.Hostname(), "duckduckgo.com") && strings.HasPrefix(u.Path, "/l/"):
		if target := u.Query().Get("uddg"); target != "" {
			return target, true
		}

	// Bing: /ck/a?...&u=a1<base64url of the destination>
	case strings.HasSuffix(u.Hostname(), "bing.com") && strings.HasPrefix(u.Path, "/ck/a"):
		if target, ok := decodeBingTarget(u.Query().Get("u")); ok {
			return target, true
		}

	// Google's older shape, still served in some layouts: /url?q=<destination>
	case isGoogleHost(u.Hostname()) && u.Path == "/url":
		for _, key := range []string{"q", "url"} {
			if target := u.Query().Get(key); strings.HasPrefix(target, "http") {
				return target, true
			}
		}

	// Google's current shape: /goto?url=<opaque blob>. Nothing in the blob is
	// readable locally, so it has to be resolved over the network.
	case isGoogleHost(u.Hostname()) && u.Path == "/goto":
		if !s.Search.ResolvesRedirects() {
			return raw, false
		}
		if target, ok := s.resolve(ctx, raw); ok {
			return target, true
		}
	}
	return raw, false
}

// decodeBingTarget decodes the "a1"-prefixed base64url payload Bing puts its
// destination in.
func decodeBingTarget(value string) (string, bool) {
	if !strings.HasPrefix(value, "a1") {
		return "", false
	}
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(value[2:], "="))
	if err != nil {
		return "", false
	}
	target := string(decoded)
	if !strings.HasPrefix(target, "http") {
		return "", false
	}
	return target, true
}

func isGoogleHost(host string) bool {
	return host == "google.com" || strings.HasSuffix(host, ".google.com")
}

// resolve asks the engine where a tracker URL goes and reads the answer out of
// the Location header, without following it.
func (s *Scraper) resolve(ctx context.Context, raw string) (string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return "", false
	}
	req.Header.Set("User-Agent", browserUserAgent)
	resp, err := s.redirects.Do(req)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close() //nolint:errcheck
	if resp.StatusCode < 300 || resp.StatusCode > 399 {
		return "", false
	}
	target := resp.Header.Get("Location")
	if !strings.HasPrefix(target, "http") {
		return "", false
	}
	return target, true
}

// UnwrapAll resolves a page of results in parallel. Sequentially this would be
// one network round trip per result on top of the search itself, which turns a
// fast tool into a slow one for no reason: the resolutions are independent.
//
// Concurrency is bounded, both to be a good citizen and because a burst of
// simultaneous requests to the engine that just answered is the kind of thing
// that gets an address rate-limited.
func (s *Scraper) UnwrapAll(ctx context.Context, results []SearchResult) {
	if !s.Search.ResolvesRedirects() {
		return
	}
	const parallel = 6
	slots := make(chan struct{}, parallel)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(r *SearchResult) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			target, ok := s.Unwrap(ctx, r.URL)
			r.URL = target
			r.Redirected = !ok && isTracker(target)
		}(&results[i])
	}
	wg.Wait()
}

// isTracker reports whether a URL is still one of the click wrappers, which is
// what Redirected reports and what a caller should not treat as a destination.
func isTracker(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil {
		return false
	}
	host := u.Hostname()
	switch {
	case isGoogleHost(host):
		return u.Path == "/goto" || u.Path == "/url"
	case strings.HasSuffix(host, "bing.com"):
		return strings.HasPrefix(u.Path, "/ck/a")
	case strings.HasSuffix(host, "duckduckgo.com"):
		return strings.HasPrefix(u.Path, "/l/")
	}
	return false
}

// finish trims a result set to the requested size and numbers it. The engines
// deliberately over-fetch - a page carries ads, "people also ask" blocks and
// video carousels that look like results until you read them - so trimming
// here rather than in each engine is what makes num_results mean the same
// thing everywhere.
func finish(resp *SearchResponse, q SearchQuery) *SearchResponse {
	results := resp.Results
	if len(results) > q.NumResults {
		results = results[:q.NumResults]
	}
	offset := q.Offset()
	for i := range results {
		results[i].Rank = offset + i + 1
	}
	resp.Results = results
	resp.Count = len(results)
	resp.Query = q.Query
	if q.Page > 1 {
		resp.Page = q.Page
	}
	return resp
}

// note records a constraint the engine could not honour.
func (r *SearchResponse) note(format string, args ...any) {
	r.Notes = append(r.Notes, fmt.Sprintf(format, args...))
}
