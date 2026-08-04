package main

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"golang.org/x/net/html"
)

// The engines in this file answer a plain HTTP request. No browser is started,
// nothing is rendered, and a machine with no Chromium installed can still
// search - which also makes them the fallback worth reaching for when Google
// has decided this address is a robot.

// ---------------------------------------------------------------------------
// DuckDuckGo
// ---------------------------------------------------------------------------

// duckDuckGoEngine reads the HTML endpoint DuckDuckGo maintains for clients
// that do not run JavaScript. It is the most cooperative source here: server
// rendered, stable markup, and the destination URL sitting in plain sight in
// the uddg parameter of the click wrapper, so unwrapping costs no extra
// request. When Google is blocked, this is what to use.
type duckDuckGoEngine struct{}

func (duckDuckGoEngine) Name() string { return EngineDuckDuckGo }

func (duckDuckGoEngine) Description() string {
	return "DuckDuckGo, through its no-JavaScript HTML endpoint. Needs no browser, " +
		"is rarely rate-limited, and returns clean destination URLs. The best fallback."
}

func (duckDuckGoEngine) NeedsBrowser() bool { return false }

func (e duckDuckGoEngine) Search(ctx context.Context, s *Scraper, q SearchQuery) (*SearchResponse, error) {
	params := url.Values{}
	params.Set("q", q.Text())
	params.Set("kl", ddgRegion(q))
	if df, ok := ddgTimeFilter[q.TimeRange]; ok {
		params.Set("df", df)
	}
	if q.SafeSearch {
		params.Set("kp", "1")
	}
	if offset := q.Offset(); offset > 0 {
		// The HTML endpoint pages by result offset rather than page number.
		params.Set("s", strconv.Itoa(offset))
	}

	resp := &SearchResponse{Engine: e.Name()}
	if q.TimeRange == "past_hour" {
		// The shortest window DuckDuckGo offers is a day. Widening is better
		// than dropping the constraint silently, and saying so is better than
		// either.
		params.Set("df", "d")
		resp.note("DuckDuckGo's shortest time filter is a day, so past_hour was widened to past_day")
	} else if q.TimeRange != "" {
		if _, ok := ddgTimeFilter[q.TimeRange]; !ok {
			resp.note("DuckDuckGo has no %q filter; the results are unfiltered by time", q.TimeRange)
		}
	}

	body, err := s.Get(ctx, "https://html.duckduckgo.com/html/?"+params.Encode())
	if err != nil {
		return nil, err
	}
	resp.Results = parseDuckDuckGo(body, q.NumResults+4)
	s.UnwrapAll(ctx, resp.Results)
	return finish(resp, q), nil
}

// parseDuckDuckGo reads results out of the HTML endpoint's markup. It is
// separate from the fetch so it can be tested against a fixed page: the
// parsing is the part that breaks when the markup changes, and a test that
// needs DuckDuckGo to be reachable does not cover it any better.
func parseDuckDuckGo(body string, want int) []SearchResult {
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		// html.Parse recovers from anything a server can send; an error here
		// means the reader failed, and there is nothing to salvage.
		return nil
	}
	var results []SearchResult
	for _, link := range findAll(doc, byClass("a", "result__a")) {
		if len(results) >= want {
			break
		}
		title := nodeText(link)
		href := absolute("https://duckduckgo.com", attr(link, "href"))
		if title == "" || href == "" {
			continue
		}
		// The snippet and the display URL are siblings of the link's own
		// block, so the search starts from the enclosing result element.
		result := SearchResult{Title: title, URL: href}
		if box := enclosing(link, "result"); box != nil {
			if snippet := find(box, byClass("", "result__snippet")); snippet != nil {
				result.Snippet = nodeText(snippet)
			}
			if display := find(box, byClass("", "result__url")); display != nil {
				result.DisplayURL = nodeText(display)
			}
		}
		results = append(results, result)
	}
	return results
}

// ddgRegion maps a region code onto DuckDuckGo's own locale codes, which pair
// a country with a language rather than naming either alone.
func ddgRegion(q SearchQuery) string {
	region := strings.ToLower(q.Region)
	language := strings.ToLower(q.Language)
	if region == "" {
		return "us-en"
	}
	if language == "" {
		language = "en"
	}
	return region + "-" + language
}

// enclosing climbs to the nearest ancestor carrying a class, which is how a
// result's snippet is reached from its link.
func enclosing(n *html.Node, class string) *html.Node {
	for p := n.Parent; p != nil; p = p.Parent {
		if p.Type == html.ElementNode && hasClass(p, class) {
			return p
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Bing
// ---------------------------------------------------------------------------

// bingEngine reads bing.com/search. Bing renders its results server-side, so
// no browser is needed, and it wraps each link in a /ck/a tracker whose target
// is base64 in the u parameter - decodable locally, so unwrapping is free here
// too.
//
// Bing is the least consistent of the three: it sometimes answers a bare page
// to a client it does not recognise, which comes back as an empty result set
// rather than an error, since an engine finding nothing and an engine refusing
// to answer look identical from here.
type bingEngine struct{}

func (bingEngine) Name() string { return EngineBing }

func (bingEngine) Description() string {
	return "Bing. Needs no browser and indexes differently from Google, which makes " +
		"it a useful second opinion rather than only a fallback."
}

func (bingEngine) NeedsBrowser() bool { return false }

func (e bingEngine) Search(ctx context.Context, s *Scraper, q SearchQuery) (*SearchResponse, error) {
	params := url.Values{}
	params.Set("q", q.Text())
	params.Set("count", strconv.Itoa(q.NumResults+5))
	params.Set("setlang", "en")
	if offset := q.Offset(); offset > 0 {
		// Bing's "first" is one-based over results.
		params.Set("first", strconv.Itoa(offset+1))
	}
	if q.Region != "" {
		params.Set("cc", strings.ToUpper(q.Region))
	}
	if freshness, ok := bingFreshness[q.TimeRange]; ok {
		params.Set("filters", "ex1:\"ez"+bingFreshnessCode(freshness)+"\"")
	}
	if q.SafeSearch {
		params.Set("adlt", "strict")
	}

	body, err := s.Get(ctx, "https://www.bing.com/search?"+params.Encode())
	if err != nil {
		return nil, err
	}
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("parsing the results page: %w", err)
	}

	resp := &SearchResponse{Engine: e.Name()}
	if q.TimeRange != "" {
		if _, ok := bingFreshness[q.TimeRange]; !ok {
			resp.note("Bing has no %q filter; the results are unfiltered by time", q.TimeRange)
		}
	}

	for _, item := range findAll(doc, byClass("li", "b_algo")) {
		if len(resp.Results) >= q.NumResults+4 {
			break
		}
		heading := find(item, func(n *html.Node) bool { return n.Data == "h2" })
		if heading == nil {
			continue
		}
		link := find(heading, func(n *html.Node) bool { return n.Data == "a" && attr(n, "href") != "" })
		if link == nil {
			continue
		}
		result := SearchResult{Title: nodeText(heading), URL: attr(link, "href")}
		if cite := find(item, func(n *html.Node) bool { return n.Data == "cite" }); cite != nil {
			result.DisplayURL = nodeText(cite)
		}
		// The caption holds the snippet; the first paragraph inside it is the
		// text, the rest is metadata Bing appends.
		if caption := find(item, byClass("", "b_caption")); caption != nil {
			if p := find(caption, func(n *html.Node) bool { return n.Data == "p" }); p != nil {
				result.Snippet = nodeText(p)
			}
		}
		resp.Results = append(resp.Results, result)
	}

	s.UnwrapAll(ctx, resp.Results)
	return finish(resp, q), nil
}

// bingFreshnessCode is the digit Bing's filter expression uses for each window.
func bingFreshnessCode(freshness string) string {
	switch freshness {
	case "Day":
		return "1"
	case "Week":
		return "2"
	default:
		return "3"
	}
}

// ---------------------------------------------------------------------------
// Mojeek
// ---------------------------------------------------------------------------

// mojeekEngine reads mojeek.com, which runs its own crawler rather than
// reselling one of the majors. That is the reason to have it: when Google,
// Bing and DuckDuckGo agree, they may only be agreeing about one index.
//
// It links straight to the destination with no tracker at all, so its URLs
// need no unwrapping. It will serve a CAPTCHA to traffic it dislikes, which is
// reported like any other block.
type mojeekEngine struct{}

func (mojeekEngine) Name() string { return EngineMojeek }

func (mojeekEngine) Description() string {
	return "Mojeek, an independent crawler with its own index. Needs no browser and " +
		"links directly to results. Worth reaching for when the other engines agree " +
		"and you want a source that is not derived from them."
}

func (mojeekEngine) NeedsBrowser() bool { return false }

func (e mojeekEngine) Search(ctx context.Context, s *Scraper, q SearchQuery) (*SearchResponse, error) {
	params := url.Values{}
	params.Set("q", q.Text())
	if offset := q.Offset(); offset > 0 {
		params.Set("s", strconv.Itoa(offset))
	}
	if q.Region != "" {
		params.Set("reg", strings.ToLower(q.Region))
	}
	if q.SafeSearch {
		params.Set("safe", "1")
	}

	target := "https://www.mojeek.com/search?" + params.Encode()
	body, err := s.mojeekFetch(ctx, target)
	if err != nil {
		return nil, err
	}
	if isMojeekChallenge(body) {
		return nil, ErrMojeekChallenge
	}

	resp := &SearchResponse{Engine: e.Name()}
	if q.TimeRange != "" {
		resp.note("Mojeek has no time filter; the results are unfiltered by time")
	}
	resp.Results = parseMojeek(body, q.NumResults+4)
	if len(resp.Results) == 0 && !isMojeekResultsPage(body) {
		// Neither results nor the known gate: a datacenter address is sometimes
		// served a bare refusal page. Its title says so independently of the
		// result selectors, so it is reported as a refusal and not as a parser
		// fault.
		return nil, fmt.Errorf("mojeek served a page that is not a results page (title %q): this "+
			"address may be rate-limited or refused", pageTitle(body))
	}
	return finish(resp, q), nil
}

// mojeekFetch gets the results page, over plain HTTP where it can and through
// the browser where that has been challenged.
//
// Mojeek gates traffic it does not recognise behind a one-time proof-of-work
// challenge and remembers the pass in a cookie for about a month. A bare HTTP
// client can never satisfy it: the challenge is solved in JavaScript, and this
// server does not solve it - working around a site's anti-bot measure is not
// something to automate.
//
// What it does instead is reuse a pass that already exists. With --cdp the
// browser is the operator's own, and if they have used Mojeek in it the cookie
// is right there; the request then goes out from that browser as any other
// page load would. That is not a bypass, it is the same session a person
// already established.
func (s *Scraper) mojeekFetch(ctx context.Context, target string) (string, error) {
	body, httpErr := s.Get(ctx, target)
	if httpErr == nil && !isMojeekChallenge(body) {
		return body, nil
	}
	if s.Browser == nil {
		if httpErr != nil {
			return "", httpErr
		}
		return body, nil
	}

	// The browser carries whatever cookies its profile has, which is the whole
	// point of trying it.
	page, err := s.Browser.Acquire()
	if err != nil {
		if httpErr != nil {
			return "", httpErr
		}
		return body, nil
	}
	defer page.Release()

	if err := page.Open(target); err != nil {
		if httpErr != nil {
			return "", httpErr
		}
		return body, nil
	}
	page.Settle(1200)
	rendered, err := page.Content()
	if err != nil {
		if httpErr != nil {
			return "", httpErr
		}
		return body, nil
	}
	return rendered, nil
}

// ErrMojeekChallenge is Mojeek's one-time verification gate.
//
// It is distinguished from the general block because the remedy is specific
// and small, and a caller told only "blocked" would wait for something that
// does not clear on its own: the challenge stands until a person passes it
// once, and then does not come back for a month.
var ErrMojeekChallenge = fmt.Errorf("mojeek is showing its one-time verification challenge (ALTCHA) " +
	"to this address. It does not clear by waiting: open https://www.mojeek.com/search?q=test in a " +
	"browser, tick \"I'm not a robot\", and Mojeek remembers the pass in a cookie for about a month. " +
	"Run this server with --cdp pointed at that browser and it will use the same cookie. " +
	"Meanwhile duckduckgo and bing need no browser and are not gated")

// isMojeekChallenge reports whether a page is the verification gate rather
// than results. The title is checked as well as the widget, since the widget's
// element name could plausibly change while the page title would not.
func isMojeekChallenge(body string) bool {
	lower := strings.ToLower(body)
	return strings.Contains(lower, "<title>captcha") ||
		strings.Contains(lower, "altcha") ||
		strings.Contains(lower, "verification required") ||
		strings.Contains(lower, "i'm not a robot")
}

// isMojeekResultsPage reports whether a page is a search results page at all,
// judged by its title ("<query> - Mojeek Search") and not by result markup, so
// a selector change still shows up as a parser fault.
func isMojeekResultsPage(body string) bool {
	return strings.Contains(strings.ToLower(pageTitle(body)), "mojeek search")
}

// pageTitle returns the text of a page's <title>, or "" if it has none.
func pageTitle(body string) string {
	lower := strings.ToLower(body)
	start := strings.Index(lower, "<title>")
	if start < 0 {
		return ""
	}
	start += len("<title>")
	end := strings.Index(lower[start:], "</title>")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(html.UnescapeString(body[start : start+end]))
}

// parseMojeek reads results out of a Mojeek results page.
//
// It selects on structure rather than on a class, and that is the whole fix
// here: this parser previously looked for li.r, and Mojeek gives each result
// its rank as a class instead - r1, r2, r3 - so it matched nothing at all and
// reported every search as finding nothing. What is stable is the shape, an
// <li> holding an <h2> with the title link.
//
// The container is not stable either. Mojeek serves ul.results-standard on
// the layout this was written against and a differently named list on others,
// so the container is a hint and not a requirement: when no list with a
// results-ish class is found, every <li> on the page is considered and the
// shape of a result is what selects it. Navigation and menu items have no
// heading link, so they fall out on their own.
//
// The hrefs are the destinations themselves, with no redirect wrapper, which
// is why nothing here needs unwrapping.
func parseMojeek(body string, want int) []SearchResult {
	doc, err := html.Parse(strings.NewReader(body))
	if err != nil {
		return nil
	}

	items := mojeekItems(doc)
	var results []SearchResult
	for _, item := range items {
		if len(results) >= want {
			break
		}
		// The title link, which is also where the destination is.
		link := find(item, func(n *html.Node) bool {
			return n.Data == "a" && hasClass(n, "title") && attr(n, "href") != ""
		})
		if link == nil {
			// Older and narrower layouts put the title link straight in the
			// heading with no class on it.
			if heading := mojeekHeading(item); heading != nil {
				link = find(heading, func(n *html.Node) bool { return n.Data == "a" && attr(n, "href") != "" })
			}
		}
		if link == nil {
			continue
		}
		title := nodeText(link)
		href := absolute("https://www.mojeek.com", attr(link, "href"))
		if title == "" || href == "" || !strings.HasPrefix(href, "http") {
			continue
		}

		result := SearchResult{Title: title, URL: href}
		if snippet := find(item, byClass("p", "s")); snippet != nil {
			result.Snippet = nodeText(snippet)
		}
		if display := find(item, byClass("span", "url")); display != nil {
			result.DisplayURL = nodeText(display)
		}
		results = append(results, result)
	}
	return results
}

// mojeekItems returns the <li> elements that could be results, preferring the
// ones inside a results list and falling back to the whole document.
func mojeekItems(doc *html.Node) []*html.Node {
	list := find(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && n.Data == "ul" && strings.Contains(attr(n, "class"), "results")
	})
	scope := doc
	if list != nil {
		scope = list
	}
	return findAll(scope, func(n *html.Node) bool {
		if n.Type != html.ElementNode || n.Data != "li" {
			return false
		}
		// A result is an item with a heading in it; menus and pagination
		// have links but no heading.
		return mojeekHeading(n) != nil
	})
}

// mojeekHeading finds the result heading in an item. Mojeek has used h2 on
// the desktop layout and h3 elsewhere, and neither is worth depending on
// alone.
func mojeekHeading(item *html.Node) *html.Node {
	return find(item, func(n *html.Node) bool {
		return n.Type == html.ElementNode && (n.Data == "h2" || n.Data == "h3")
	})
}
