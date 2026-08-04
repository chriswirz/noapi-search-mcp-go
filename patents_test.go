package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
)

// The fixture is a real response from Google Patents' query endpoint, trimmed
// to the fields this server reads. It is checked in rather than fetched
// because the endpoint throttles hard per address: a test that calls it fails
// on a shared CI runner for reasons that have nothing to do with the parsing,
// and the parsing is the part that actually breaks.
const patentsFixture = "testdata/patents_query.json"

func TestPatentQueryString(t *testing.T) {
	for _, tc := range []struct {
		name string
		q    PatentQuery
		want map[string]string
	}{
		{
			name: "plain query",
			q:    PatentQuery{Query: "solid state battery", NumResults: 10, Page: 1},
			want: map[string]string{"q": "solid state battery", "num": "10"},
		},
		{
			name: "every filter",
			q: PatentQuery{
				Query: "battery", Assignee: "Tesla", Inventor: "Marcus Stack",
				Country: "US,EP", Status: "GRANT", Language: "ENGLISH",
				After: "20200101", Before: "20241231", DateType: "priority",
				Sort: "new", NumResults: 5, Page: 3,
			},
			want: map[string]string{
				"q": "battery", "assignee": "Tesla", "inventor": "Marcus Stack",
				"country": "US,EP", "status": "GRANT", "language": "ENGLISH",
				// The date filters name the field they apply to.
				"after": "priority:20200101", "before": "priority:20241231",
				"sort": "new", "num": "5",
				// The endpoint pages from zero, the tool from one.
				"page": "2",
			},
		},
		{
			name: "filing dates",
			q:    PatentQuery{Query: "x", DateType: "filing", After: "20150101", NumResults: 10, Page: 1},
			want: map[string]string{"after": "filing:20150101"},
		},
		{
			// Page one must not send a page parameter at all, or the endpoint
			// is being told something the caller did not ask for.
			name: "first page sends no page",
			q:    PatentQuery{Query: "x", NumResults: 10, Page: 1},
			want: map[string]string{"q": "x"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parsed, err := url.ParseQuery(patentsQueryString(tc.q))
			if err != nil {
				t.Fatalf("the query string does not parse: %v", err)
			}
			for key, want := range tc.want {
				if got := parsed.Get(key); got != want {
					t.Errorf("%s = %q, want %q", key, got, want)
				}
			}
			if tc.name == "first page sends no page" {
				if _, ok := parsed["page"]; ok {
					t.Error("page was sent for the first page")
				}
			}
			for _, empty := range []string{"assignee", "inventor", "status", "sort"} {
				if tc.q.Assignee == "" && empty == "assignee" && parsed.Get(empty) != "" {
					t.Errorf("%s was sent though it was not set", empty)
				}
			}
		})
	}
}

func TestPatentArgsValidation(t *testing.T) {
	cfg := SearchConfig{MaxResults: 20}
	for _, tc := range []struct {
		name    string
		args    patentArgs
		wantErr bool
		check   func(PatentQuery) string
	}{
		{
			name:    "nothing to search for",
			args:    patentArgs{},
			wantErr: true,
		},
		{
			// Searching an assignee's whole portfolio is a real question, and
			// it needs no keywords.
			name:  "assignee alone is enough",
			args:  patentArgs{Assignee: "Genentech"},
			check: func(q PatentQuery) string { return "" },
		},
		{
			name:  "inventor alone is enough",
			args:  patentArgs{Inventor: "Shinya Yamanaka"},
			check: func(q PatentQuery) string { return "" },
		},
		{
			name:    "bad status",
			args:    patentArgs{Query: "x", Status: "maybe"},
			wantErr: true,
		},
		{
			name:    "bad sort",
			args:    patentArgs{Query: "x", Sort: "sideways"},
			wantErr: true,
		},
		{
			name:    "bad date type",
			args:    patentArgs{Query: "x", DateType: "expiry"},
			wantErr: true,
		},
		{
			name:    "not a real date",
			args:    patentArgs{Query: "x", After: "2020-13-45"},
			wantErr: true,
		},
		{
			name:    "unparseable date",
			args:    patentArgs{Query: "x", Before: "last tuesday"},
			wantErr: true,
		},
		{
			// A window that matches nothing is a mistake worth reporting,
			// because the endpoint answers it with an empty result set that
			// reads exactly like "this art does not exist".
			name:    "inverted date window",
			args:    patentArgs{Query: "x", After: "2020-01-01", Before: "2015-01-01"},
			wantErr: true,
		},
		{
			name: "dates normalised",
			args: patentArgs{Query: "x", After: "2015-06-01", Before: "20201231"},
			check: func(q PatentQuery) string {
				if q.After != "20150601" {
					return "after was not normalised: " + q.After
				}
				if q.Before != "20201231" {
					return "before was not normalised: " + q.Before
				}
				return ""
			},
		},
		{
			// "after 2015" means the first of January to whoever typed it.
			name: "bare year",
			args: patentArgs{Query: "x", After: "2015"},
			check: func(q PatentQuery) string {
				if q.After != "20150101" {
					return "a bare year became " + q.After
				}
				return ""
			},
		},
		{
			name: "defaults",
			args: patentArgs{Query: " widgets "},
			check: func(q PatentQuery) string {
				if q.Query != "widgets" {
					return "the query was not trimmed"
				}
				if q.NumResults != 10 {
					return "num_results did not take its default"
				}
				if q.Page != 1 {
					return "page did not default to 1"
				}
				if q.DateType != "priority" {
					return "date_type did not default to priority"
				}
				return ""
			},
		},
		{
			name: "synonyms accepted",
			args: patentArgs{Query: "x", Status: "granted", Sort: "newest", Country: "us"},
			check: func(q PatentQuery) string {
				if q.Status != "GRANT" {
					return "status = " + q.Status
				}
				if q.Sort != "new" {
					return "sort = " + q.Sort
				}
				if q.Country != "US" {
					return "country = " + q.Country
				}
				return ""
			},
		},
		{
			name: "num_results clamped",
			args: patentArgs{Query: "x", NumResults: 500},
			check: func(q PatentQuery) string {
				if q.NumResults != 20 {
					return "num_results was not clamped"
				}
				return ""
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q, err := tc.args.build(cfg)
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
			if problem := tc.check(q); problem != "" {
				t.Error(problem)
			}
		})
	}
}

// TestPatentsParsesRealResponse runs the whole path - fetch, decode, clean -
// against a stub serving a captured response.
func TestPatentsParsesRealResponse(t *testing.T) {
	fixture, err := os.ReadFile(patentsFixture)
	if err != nil {
		t.Fatalf("reading the fixture: %v", err)
	}
	stub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The inner query travels URL-encoded inside the url parameter.
		inner := r.URL.Query().Get("url")
		if inner == "" {
			t.Error("the query was not sent in the url parameter")
		}
		if !strings.Contains(inner, "assignee=Tesla") {
			t.Errorf("the assignee filter did not reach the endpoint: %q", inner)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer stub.Close()

	s := NewScraper(nil, SearchConfig{MaxResults: 20})
	q, err := patentArgs{Query: "battery", Assignee: "Tesla", NumResults: 3}.build(s.Search)
	if err != nil {
		t.Fatal(err)
	}

	body, err := s.Get(context.Background(),
		stub.URL+"?url="+url.QueryEscape(patentsQueryString(q))+"&exp=")
	if err != nil {
		t.Fatalf("fetching the stub: %v", err)
	}
	resp, err := decodePatents(body, q)
	if err != nil {
		t.Fatalf("decoding: %v", err)
	}

	if resp.TotalMatches != 410 {
		t.Errorf("total_matches = %d, want 410", resp.TotalMatches)
	}
	if resp.Count != 3 {
		t.Fatalf("count = %d, want 3", resp.Count)
	}

	first := resp.Patents[0]
	if first.Number != "US12497105B2" {
		t.Errorf("number = %q", first.Number)
	}
	// The endpoint wraps matched terms in <b> and pads fields with a leading
	// space. A caller comparing this against a list of company names would be
	// comparing "<b>Tesla</b>, Inc." otherwise.
	if first.Assignee != "Tesla, Inc." {
		t.Errorf("assignee = %q, want the markup stripped", first.Assignee)
	}
	if first.Title != "Integrated components for vehicles" {
		t.Errorf("title = %q, want it trimmed", first.Title)
	}
	if strings.Contains(first.Snippet, "<b>") || strings.Contains(first.Snippet, "&hellip;") {
		t.Errorf("the snippet still carries markup: %q", first.Snippet)
	}
	if !strings.HasSuffix(first.Snippet, "…") {
		t.Errorf("the HTML entity was not decoded: %q", first.Snippet[max(0, len(first.Snippet)-20):])
	}
	if first.GrantDate != "2025-12-16" || !first.Granted {
		t.Errorf("grant date %q, granted=%v", first.GrantDate, first.Granted)
	}
	if first.URL != "https://patents.google.com/patent/US12497105B2/en" {
		t.Errorf("url = %q", first.URL)
	}
	// Not every document has a published PDF - the field is empty for plenty
	// of them, including this one - so the assertion is that a PDF URL, when
	// there is one, is a real URL rather than a bare storage path.
	for _, p := range resp.Patents {
		if p.PDFURL != "" && !strings.HasPrefix(p.PDFURL, "https://patentimages.storage.googleapis.com/") {
			t.Errorf("%s: pdf url = %q", p.Number, p.PDFURL)
		}
	}
	for i, p := range resp.Patents {
		if p.Rank != i+1 {
			t.Errorf("patent %d has rank %d", i, p.Rank)
		}
	}

	// The rendering is what a model actually reads, so it has to carry the
	// identifying facts rather than only the title.
	rendered := resp.Render()
	for _, want := range []string{"US12497105B2", "granted", "Tesla, Inc.", "of 410 matching"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the rendered result does not mention %q", want)
		}
	}
}

// TestPatentsEmptyResultIsExplained checks that nothing-found says why it might
// be nothing-found. An over-narrow filter and a genuine absence of prior art
// produce the same empty list, and only one of them is a finding.
func TestPatentsEmptyResultIsExplained(t *testing.T) {
	const empty = `{"results":{"total_num_results":0,"cluster":[{}]}}`
	q, err := patentArgs{Query: "nothing matches this"}.build(SearchConfig{MaxResults: 20})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := decodePatents(empty, q)
	if err != nil {
		t.Fatalf("decoding an empty result: %v", err)
	}
	if resp.Count != 0 || len(resp.Patents) != 0 {
		t.Fatalf("count = %d", resp.Count)
	}
	if len(resp.Notes) == 0 {
		t.Fatal("an empty result carries no explanation")
	}
	if !strings.Contains(resp.Render(), "0 patents") {
		t.Error("the rendering does not say the result was empty")
	}
}

func TestPatentsRejectsNonJSON(t *testing.T) {
	q, _ := patentArgs{Query: "x"}.build(SearchConfig{MaxResults: 20})
	// A throttle or an interstitial answers with HTML, which must be reported
	// as such rather than decoded into an empty result set that reads like
	// "nothing matched".
	if _, err := decodePatents("<html><title>Error 503</title></html>", q); err == nil {
		t.Fatal("an HTML body was accepted as a result")
	}
}

func TestPatentText(t *testing.T) {
	for in, want := range map[string]string{
		" <b>Tesla</b>, Inc.":     "Tesla, Inc.",
		"A method &amp; device":   "A method & device",
		" trailing text &hellip;": "trailing text …",
		"  collapsed   spaces  ":  "collapsed spaces",
		"":                        "",
	} {
		if got := patentText(in); got != want {
			t.Errorf("patentText(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPatentPDFURL(t *testing.T) {
	const path = "5d/e4/5b/ea4b020ea87757/CN110210378B.pdf"
	if got, want := patentPDFURL(path), "https://patentimages.storage.googleapis.com/"+path; got != want {
		t.Errorf("patentPDFURL = %q, want %q", got, want)
	}
	// An empty path means no PDF was published, which is common for
	// applications and must not become a URL to nowhere.
	if got := patentPDFURL(""); got != "" {
		t.Errorf("an empty path became %q", got)
	}
}

func TestWithThousands(t *testing.T) {
	for in, want := range map[int]string{
		0: "0", 42: "42", 999: "999", 1000: "1,000",
		124834: "124,834", 1234567: "1,234,567",
	} {
		if got := withThousands(in); got != want {
			t.Errorf("withThousands(%d) = %q, want %q", in, got, want)
		}
	}
}

// TestPatentsToolIsRegistered guards the wiring, since the operation is what
// puts it on both surfaces at once.
func TestPatentsToolIsRegistered(t *testing.T) {
	srv, cfg := testServer(t)
	if !contains(srv.ToolNames(), "google_patents") {
		t.Fatal("google_patents is not registered as a tool")
	}
	spec := buildSpec(cfg)
	paths := spec["paths"].(map[string]any)
	if _, ok := paths[cfg.API.BasePath+"/search/patents"]; !ok {
		t.Error("the patent search is not in the OpenAPI document")
	}
	var found *Operation
	for _, op := range Operations() {
		if op.Name == "google_patents" {
			found = op
		}
	}
	if found == nil {
		t.Fatal("the operation is missing")
	}
	// The endpoint returns JSON rather than a rendered page, so unlike every
	// other Google tool here this one needs no browser on the common path.
	if !found.ReadOnly {
		t.Error("patent search is not marked read-only")
	}
	props := found.Schema["properties"].(map[string]any)
	for _, want := range []string{"query", "assignee", "inventor", "status", "before", "after", "date_type"} {
		if _, ok := props[want]; !ok {
			t.Errorf("the schema has no %q property", want)
		}
	}
}

// TestFetchInPageJSONShape checks the contract between the fallback's script
// and the Go struct that decodes it, which is easy to break silently.
func TestFetchInPageJSONShape(t *testing.T) {
	var out struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal([]byte(`{"status":200,"body":"{}","error":""}`), &out); err != nil {
		t.Fatal(err)
	}
	if out.Status != 200 || out.Body != "{}" {
		t.Errorf("decoded %+v", out)
	}
	for _, field := range []string{"status", "body", "error"} {
		if !strings.Contains(fetchInPageJS, field+":") && !strings.Contains(fetchInPageJS, field+" :") {
			t.Errorf("the fetch script does not set %q", field)
		}
	}
}
