package main

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

func init() {
	registerOperation(&Operation{
		Name:     "google_patents",
		Title:    "Patent search",
		Path:     "/search/patents",
		ReadOnly: true,
		Summary:  "Search Google Patents by subject, inventor, assignee, date and jurisdiction.",
		Description: `Search patents and published applications on Google Patents.

Each result comes back with its publication number, title, abstract snippet,
inventor, assignee, the priority, filing, publication and grant dates, and links
to the patent page and its PDF.

Filters worth knowing, because a bare keyword search over 150 million documents
is rarely the question anyone actually has:
  - assignee is the company or institution that owns it, inventor the person.
  - status separates granted patents from published applications, which is the
    difference between a right that exists and one that was asked for.
  - country restricts the jurisdiction, and a patent is only enforceable in the
    one that granted it.
  - before and after filter on a date, and which date matters: "priority" is
    when the idea was first claimed anywhere and is what decides prior art,
    while "publication" and "filing" are administrative.

Sample prompts:
  - "Find patents on solid state battery electrolytes"
  - "What has Tesla patented about battery cooling since 2020?"
  - "Search for patents by Sebastian Thrun on lidar"
  - "Any granted US patents on CRISPR gene drives?"
  - "Show me prior art for adaptive bitrate video streaming before 2010"

This reads Google Patents' own JSON endpoint rather than scraping a page, so it
needs no browser and is the most reliable search tool here. It is still a
search, not a legal opinion: a keyword query does not establish freedom to
operate, and the absence of a result is not evidence that nothing exists.`,
		Schema: objectSchema([]string{"query"}, map[string]any{
			"query": stringProp("What to search for. Google Patents' own syntax works: quoted " +
				"phrases, OR, and parentheses."),
			"num_results": intProp("How many results to return.", 10, 1, 50),
			"page":        intProp("Results page, from 1.", 1, 1, 20),
			"inventor":    stringProp("Restrict to an inventor, e.g. \"Shinya Yamanaka\"."),
			"assignee": stringProp("Restrict to the owner, e.g. \"Genentech\", \"Tesla\". " +
				"This is the company or institution rather than the person."),
			"country": stringProp("Comma-separated jurisdiction codes, e.g. \"US\", \"US,EP,WO\". " +
				"A patent is only enforceable where it was granted."),
			"status": enumProp("Granted patents, or published applications that may never be granted.",
				[]string{"GRANT", "APPLICATION"}, ""),
			"language": stringProp("Comma-separated document languages, e.g. \"ENGLISH\", \"ENGLISH,GERMAN\"."),
			"before": stringProp("Only documents before this date, as YYYY-MM-DD or YYYYMMDD. " +
				"Combine with date_type."),
			"after": stringProp("Only documents on or after this date, as YYYY-MM-DD or YYYYMMDD."),
			"date_type": enumProp("Which date before and after apply to. Priority is when the idea "+
				"was first claimed anywhere, and is the one that decides prior art.",
				[]string{"priority", "filing", "publication"}, "priority"),
			"sort": enumProp("Result order. Relevance is the default; new and old sort by date, "+
				"which is what you want when the question is about what came first.",
				[]string{"relevance", "new", "old"}, "relevance"),
		}),
		Run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var args patentArgs
			if err := bind(raw, &args); err != nil {
				return nil, err
			}
			query, err := args.build(s.cfg.Search)
			if err != nil {
				return nil, err
			}
			return s.scraper.patents(ctx, query)
		},
	})
}

// patentArgs are the tool's arguments before validation.
type patentArgs struct {
	Query      string `json:"query"`
	NumResults int    `json:"num_results"`
	Page       int    `json:"page"`
	Inventor   string `json:"inventor"`
	Assignee   string `json:"assignee"`
	Country    string `json:"country"`
	Status     string `json:"status"`
	Language   string `json:"language"`
	Before     string `json:"before"`
	After      string `json:"after"`
	DateType   string `json:"date_type"`
	Sort       string `json:"sort"`
}

// PatentQuery is a validated patent search.
type PatentQuery struct {
	Query      string
	NumResults int
	Page       int
	Inventor   string
	Assignee   string
	Country    string
	Status     string
	Language   string
	Before     string // already normalised to YYYYMMDD
	After      string
	DateType   string
	Sort       string
}

// build validates the arguments. Everything that can be wrong is caught here
// rather than being passed through to Google, because Google answers a bad
// filter with zero results rather than an error - and "no patents match" is a
// meaningful, misleading answer to a query that was never run properly.
func (a patentArgs) build(cfg SearchConfig) (PatentQuery, error) {
	if strings.TrimSpace(a.Query) == "" && a.Inventor == "" && a.Assignee == "" {
		return PatentQuery{}, badInput("query is required, or search by inventor or assignee alone")
	}
	page := a.Page
	if page <= 0 {
		page = 1
	}
	if page > 20 {
		return PatentQuery{}, badInput("page %d: Google Patents stops paging long before this; ask for at most 20", page)
	}

	q := PatentQuery{
		Query:      strings.TrimSpace(a.Query),
		NumResults: clampResults(a.NumResults, 10, cfg.MaxResults),
		Page:       page,
		Inventor:   strings.TrimSpace(a.Inventor),
		Assignee:   strings.TrimSpace(a.Assignee),
		Country:    strings.ToUpper(strings.TrimSpace(a.Country)),
		Language:   strings.ToUpper(strings.TrimSpace(a.Language)),
	}

	switch strings.ToUpper(strings.TrimSpace(a.Status)) {
	case "":
	case "GRANT", "GRANTED", "PATENT":
		q.Status = "GRANT"
	case "APPLICATION", "APPLICATIONS", "PENDING":
		q.Status = "APPLICATION"
	default:
		return PatentQuery{}, badInput("status %q: want GRANT or APPLICATION", a.Status)
	}

	switch strings.ToLower(strings.TrimSpace(a.Sort)) {
	case "", "relevance":
	case "new", "newest":
		q.Sort = "new"
	case "old", "oldest":
		q.Sort = "old"
	default:
		return PatentQuery{}, badInput("sort %q: want relevance, new or old", a.Sort)
	}

	q.DateType = strings.ToLower(strings.TrimSpace(a.DateType))
	switch q.DateType {
	case "":
		q.DateType = "priority"
	case "priority", "filing", "publication":
	default:
		return PatentQuery{}, badInput("date_type %q: want priority, filing or publication", a.DateType)
	}

	var err error
	if q.Before, err = patentDate(a.Before, "before"); err != nil {
		return PatentQuery{}, err
	}
	if q.After, err = patentDate(a.After, "after"); err != nil {
		return PatentQuery{}, err
	}
	if q.Before != "" && q.After != "" && q.After > q.Before {
		return PatentQuery{}, badInput("after (%s) is later than before (%s), which matches nothing", a.After, a.Before)
	}
	return q, nil
}

// patentDate normalises a date to the YYYYMMDD form the endpoint takes.
//
// Both spellings are accepted because both get typed: YYYY-MM-DD is what a
// person writes and what every other date field in this server takes, and
// YYYYMMDD is what Google Patents itself uses. A bare year is accepted too and
// means the first of January, which is what "after 2015" means to the person
// who typed it.
func patentDate(value, field string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if regexp.MustCompile(`^\d{4}$`).MatchString(value) {
		value += "-01-01"
	}
	compact := strings.ReplaceAll(value, "-", "")
	if len(compact) != 8 {
		return "", badInput("%s %q: want a date as YYYY-MM-DD, YYYYMMDD or YYYY", field, value)
	}
	if _, err := time.Parse("20060102", compact); err != nil {
		return "", badInput("%s %q: not a real date", field, value)
	}
	return compact, nil
}

// Patent is one document from the index.
type Patent struct {
	Rank int `json:"rank"`

	// Number is the publication number, e.g. "US12497105B2". It is the one
	// field that identifies the document anywhere else.
	Number string `json:"number"`

	Title   string `json:"title"`
	Snippet string `json:"snippet,omitempty"`

	Inventor string `json:"inventor,omitempty"`
	Assignee string `json:"assignee,omitempty"`

	// PriorityDate is when the invention was first claimed anywhere, and is
	// the date that decides prior art. The other three are administrative.
	PriorityDate    string `json:"priority_date,omitempty"`
	FilingDate      string `json:"filing_date,omitempty"`
	PublicationDate string `json:"publication_date,omitempty"`
	GrantDate       string `json:"grant_date,omitempty"`

	// Granted distinguishes a right that exists from one that was applied for.
	// It is derived from whether a grant date is present, which is the only
	// signal the search index carries.
	Granted bool `json:"granted"`

	URL    string `json:"url"`
	PDFURL string `json:"pdf_url,omitempty"`
}

// PatentResponse is a whole patent search answer.
type PatentResponse struct {
	Query string `json:"query"`
	Page  int    `json:"page,omitempty"`

	// Count is how many are returned here; TotalMatches is how many the index
	// holds. The gap matters: a query returning ten of four hundred thousand
	// has not been narrowed enough to conclude anything from.
	Count        int `json:"count"`
	TotalMatches int `json:"total_matches"`

	Patents []Patent `json:"patents"`
	Notes   []string `json:"notes,omitempty"`
}

func (r *PatentResponse) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d patents for %q", r.Count, r.Query)
	if r.TotalMatches > r.Count {
		fmt.Fprintf(&b, " (of %s matching)", withThousands(r.TotalMatches))
	}
	if r.Page > 1 {
		fmt.Fprintf(&b, ", page %d", r.Page)
	}
	b.WriteString("\n")
	for _, note := range r.Notes {
		fmt.Fprintf(&b, "Note: %s\n", note)
	}

	for _, p := range r.Patents {
		status := "application"
		if p.Granted {
			status = "granted"
		}
		fmt.Fprintf(&b, "\n%d. %s  [%s, %s]\n", p.Rank, p.Title, p.Number, status)
		for _, field := range []struct{ label, value string }{
			{"Assignee", p.Assignee},
			{"Inventor", p.Inventor},
			{"Priority", p.PriorityDate},
			{"Filed", p.FilingDate},
			{"Granted", p.GrantDate},
			{"Published", p.PublicationDate},
		} {
			if field.value != "" {
				fmt.Fprintf(&b, "   %-10s %s\n", field.label+":", field.value)
			}
		}
		if p.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", p.Snippet)
		}
		fmt.Fprintf(&b, "   %s\n", p.URL)
	}
	return strings.TrimRight(b.String(), "\n")
}

// withThousands groups a count for reading, since these run to seven digits.
func withThousands(n int) string {
	s := strconv.Itoa(n)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	lead := len(s) % 3
	if lead > 0 {
		b.WriteString(s[:lead])
	}
	for i := lead; i < len(s); i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

// patentsEndpoint is Google Patents' own search endpoint - the one its page
// calls to fill itself in.
//
// Using it rather than scraping the page is not a shortcut, it is the only
// sound option: patents.google.com is a Polymer application that renders
// nothing without JavaScript and whose components carry no stable hooks. This
// returns the same data as structured JSON, needs no browser, and does not
// break when the page is restyled. It makes this the most reliable search tool
// in the server, and the only one whose results are not reconstructed from
// someone else's layout.
const patentsEndpoint = "https://patents.google.com/xhr/query"

// patentsResponse is the shape the endpoint answers with. Only the fields this
// server uses are declared; the payload carries a good deal more - chemistry
// annotations, figure lists, family metadata - that nothing here needs.
type patentsResponse struct {
	Results struct {
		TotalNumResults int `json:"total_num_results"`
		Cluster         []struct {
			Result []struct {
				Patent struct {
					Title             string `json:"title"`
					Snippet           string `json:"snippet"`
					PriorityDate      string `json:"priority_date"`
					FilingDate        string `json:"filing_date"`
					GrantDate         string `json:"grant_date"`
					PublicationDate   string `json:"publication_date"`
					Inventor          string `json:"inventor"`
					Assignee          string `json:"assignee"`
					PublicationNumber string `json:"publication_number"`
					PDF               string `json:"pdf"`
				} `json:"patent"`
			} `json:"result"`
		} `json:"cluster"`
	} `json:"results"`
}

// patents runs one search against the endpoint.
func (s *Scraper) patents(ctx context.Context, q PatentQuery) (*PatentResponse, error) {
	target := patentsEndpoint + "?url=" + url.QueryEscape(patentsQueryString(q)) + "&exp="

	body, err := s.patentsFetch(ctx, target)
	if err != nil {
		return nil, err
	}
	return decodePatents(body, q)
}

// decodePatents turns the endpoint's JSON into the response this server
// returns. It is separate from the fetch so it can be tested against a
// captured payload: the decoding is the part that breaks when Google changes
// the shape, and it is the part a live call covers worst.
func decodePatents(body string, q PatentQuery) (*PatentResponse, error) {
	var decoded patentsResponse
	if err := json.Unmarshal([]byte(body), &decoded); err != nil {
		// A throttle or an interstitial answers with HTML. Reporting that as
		// a decode failure is right: silently returning an empty result set
		// would read as "no patents match", which is a different and much
		// more misleading answer.
		return nil, fmt.Errorf("google patents returned something other than the expected JSON, "+
			"which is usually a rate-limit page rather than a change to the endpoint: %w", err)
	}

	resp := &PatentResponse{
		Query:        q.describe(),
		TotalMatches: decoded.Results.TotalNumResults,
	}
	if q.Page > 1 {
		resp.Page = q.Page
	}

	for _, cluster := range decoded.Results.Cluster {
		for _, item := range cluster.Result {
			if len(resp.Patents) >= q.NumResults {
				break
			}
			p := item.Patent
			if p.PublicationNumber == "" {
				continue
			}
			resp.Patents = append(resp.Patents, Patent{
				Rank:            len(resp.Patents) + 1 + (q.Page-1)*q.NumResults,
				Number:          p.PublicationNumber,
				Title:           patentText(p.Title),
				Snippet:         patentText(p.Snippet),
				Inventor:        patentText(p.Inventor),
				Assignee:        patentText(p.Assignee),
				PriorityDate:    p.PriorityDate,
				FilingDate:      p.FilingDate,
				PublicationDate: p.PublicationDate,
				GrantDate:       p.GrantDate,
				Granted:         p.GrantDate != "",
				URL:             "https://patents.google.com/patent/" + p.PublicationNumber + "/en",
				PDFURL:          patentPDFURL(p.PDF),
			})
		}
	}
	resp.Count = len(resp.Patents)

	if resp.Count == 0 {
		resp.Notes = append(resp.Notes, "nothing matched. Google Patents answers an over-narrow "+
			"filter with no results rather than an error, so check the assignee spelling and the "+
			"date window before concluding the art does not exist.")
	} else if resp.TotalMatches > 10000 {
		resp.Notes = append(resp.Notes, fmt.Sprintf("%s documents match, so these are the top few "+
			"of a very broad query. Narrowing by assignee, date or jurisdiction will say more than "+
			"paging through this will.", withThousands(resp.TotalMatches)))
	}
	return resp, nil
}

// patentsFetch gets the endpoint's JSON, over plain HTTP where it can and
// through the browser where it cannot.
//
// The plain request is tried first because it is fast, needs nothing installed,
// and works - until it does not. Google throttles this endpoint per address
// after a handful of requests and then answers 503 to everything, which is not
// a shape a retry or a slower cadence recovers from within a session.
//
// The fallback runs the same request from inside a page on patents.google.com.
// That is the difference that matters: it is a same-origin fetch carrying the
// session cookies the site set, which is exactly what the throttle is
// distinguishing. It costs a browser, so it is the second choice rather than
// the first.
func (s *Scraper) patentsFetch(ctx context.Context, target string) (string, error) {
	body, httpErr := s.Get(ctx, target)
	if httpErr == nil {
		return body, nil
	}
	if s.Browser == nil {
		return "", fmt.Errorf("google patents: %w", httpErr)
	}

	body, browserErr := s.fetchInPage(patentsOrigin, target)
	if browserErr != nil {
		// Both routes are reported. The plain one says what Google answered,
		// and the browser one says why the fallback could not stand in for it -
		// most often that no browser is installed, which is worth knowing
		// before anyone concludes the endpoint is gone.
		//
		// When both were refused rather than failing, that is said plainly:
		// otherwise two stacked 503s read as a broken integration, and the
		// response to that is a code change rather than the wait it needs.
		if isRefusal(httpErr) && isRefusal(browserErr) {
			return "", fmt.Errorf("google patents is rate-limiting this address: %w. "+
				"It refused the browser as well, so this is the network rather than a fault, "+
				"and it clears on its own - usually in minutes. Nothing here needs changing",
				httpErr)
		}
		return "", fmt.Errorf("google patents: %w; and through the browser: %w", httpErr, browserErr)
	}
	return body, nil
}

// patentsOrigin is the page the fallback fetch is issued from.
const patentsOrigin = "https://patents.google.com/"

// fetchInPage runs a fetch from inside a page on the given origin and returns
// the response body.
//
// This is for endpoints that answer a browser and refuse a bare client. The
// request goes out with the page's cookies, its origin and its referer, none of
// which an outside client can reproduce, and it is issued by the same engine
// that would issue it if a person were using the site.
func (s *Scraper) fetchInPage(origin, target string) (string, error) {
	page, err := s.Browser.Acquire()
	if err != nil {
		return "", err
	}
	defer page.Release()

	if _, err := page.Goto(origin); err != nil {
		return "", fmt.Errorf("opening %s: %w", origin, err)
	}
	page.DismissConsent()

	var out struct {
		Status int    `json:"status"`
		Body   string `json:"body"`
		Error  string `json:"error"`
	}
	if err := page.EvaluateInto(&out, fetchInPageJS, target); err != nil {
		return "", fmt.Errorf("fetching %s from the page: %w", target, err)
	}
	if out.Error != "" {
		return "", fmt.Errorf("fetching %s from the page: %s", target, out.Error)
	}
	if out.Status != 200 {
		return "", fmt.Errorf("%s answered HTTP %d to the browser as well", hostOf(target), out.Status)
	}
	return out.Body, nil
}

// fetchInPageJS issues the request from the page's own context. The failure is
// returned rather than thrown, so a network error arrives as data this server
// can explain instead of as an opaque evaluation failure.
const fetchInPageJS = `async (target) => {
  try {
    const response = await fetch(target, {
      credentials: 'include',
      headers: { 'Accept': 'application/json' },
    });
    return { status: response.status, body: await response.text(), error: '' };
  } catch (e) {
    return { status: 0, body: '', error: String(e) };
  }
}`

// patentsQueryString builds the inner query string, which the endpoint takes
// URL-encoded inside its own url parameter. It is Google Patents' own search
// syntax: the same string that appears in the address bar of a search on the
// site, which makes a result reproducible by hand.
func patentsQueryString(q PatentQuery) string {
	params := url.Values{}
	if q.Query != "" {
		params.Set("q", q.Query)
	}
	if q.Inventor != "" {
		params.Set("inventor", q.Inventor)
	}
	if q.Assignee != "" {
		params.Set("assignee", q.Assignee)
	}
	if q.Country != "" {
		params.Set("country", q.Country)
	}
	if q.Status != "" {
		params.Set("status", q.Status)
	}
	if q.Language != "" {
		params.Set("language", q.Language)
	}
	// The date filters name their own field, so "after=priority:20150101"
	// means a priority date on or after that day.
	if q.After != "" {
		params.Set("after", q.DateType+":"+q.After)
	}
	if q.Before != "" {
		params.Set("before", q.DateType+":"+q.Before)
	}
	if q.Sort != "" {
		params.Set("sort", q.Sort)
	}
	params.Set("num", strconv.Itoa(q.NumResults))
	if q.Page > 1 {
		// The endpoint pages from zero.
		params.Set("page", strconv.Itoa(q.Page-1))
	}
	return params.Encode()
}

// describe renders the query as a phrase, so a caller reading only the response
// can see which filters were actually applied. A filter that was silently
// dropped and one that matched nothing look identical otherwise.
func (q PatentQuery) describe() string {
	parts := make([]string, 0, 6)
	if q.Query != "" {
		parts = append(parts, q.Query)
	}
	for _, field := range []struct{ label, value string }{
		{"assignee", q.Assignee},
		{"inventor", q.Inventor},
		{"country", q.Country},
		{"language", q.Language},
	} {
		if field.value != "" {
			parts = append(parts, field.label+":"+field.value)
		}
	}
	if q.Status != "" {
		parts = append(parts, strings.ToLower(q.Status))
	}
	if q.After != "" {
		parts = append(parts, q.DateType+" after "+dashedDate(q.After))
	}
	if q.Before != "" {
		parts = append(parts, q.DateType+" before "+dashedDate(q.Before))
	}
	return strings.Join(parts, ", ")
}

// dashedDate turns YYYYMMDD back into YYYY-MM-DD for display.
func dashedDate(compact string) string {
	if len(compact) != 8 {
		return compact
	}
	return compact[:4] + "-" + compact[4:6] + "-" + compact[6:]
}

// patentHighlight matches the <b> tags the endpoint wraps matched terms in.
var patentHighlight = regexp.MustCompile(`</?b>`)

// patentText cleans a field from the endpoint.
//
// The values are fragments of HTML rather than plain text: matched search terms
// come wrapped in <b>, and the abstract snippet ends in a literal &hellip;.
// Left alone they reach a model as markup, and a caller comparing an assignee
// against a list would be comparing "<b>Tesla</b>, Inc." against "Tesla, Inc.".
func patentText(s string) string {
	s = patentHighlight.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	// The endpoint pads several fields with a leading space.
	return strings.TrimSpace(strings.Join(strings.Fields(s), " "))
}

// patentPDFURL turns the storage path the endpoint returns into a URL. An empty
// path means no PDF was published, which is common for applications.
func patentPDFURL(path string) string {
	if path == "" {
		return ""
	}
	return "https://patentimages.storage.googleapis.com/" + path
}
