package main

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Brave and Startpage both refuse a plain HTTP client - Brave answers 429 to
// one almost immediately - so both go through the browser. Neither wraps its
// result links in a tracker, which means what they return needs no unwrapping
// and arrives ready to use.
//
// Both also do more to distinguish a browser a person is using from one a
// program is driving than Google does, and a fresh headless profile is the
// least convincing thing you can present them with. Expect these two to be the
// first to fail on a new install and the first to start working once the
// profile has some history in it.

// ---------------------------------------------------------------------------
// Brave
// ---------------------------------------------------------------------------

// braveEngine scrapes search.brave.com, which serves its own index.
type braveEngine struct{}

func (braveEngine) Name() string { return EngineBrave }

func (braveEngine) Description() string {
	return "Brave Search, an independent index. Links directly to results, but needs " +
		"the browser: it refuses plain HTTP clients."
}

func (braveEngine) NeedsBrowser() bool { return true }

func (e braveEngine) Search(_ context.Context, s *Scraper, q SearchQuery) (*SearchResponse, error) {
	params := url.Values{}
	params.Set("q", q.Text())
	if q.Page > 1 {
		params.Set("offset", strconv.Itoa(q.Page-1))
	}
	if q.Region != "" {
		params.Set("country", strings.ToLower(q.Region))
	}
	if q.SafeSearch {
		params.Set("safesearch", "strict")
	}
	if window, ok := braveWindow[q.TimeRange]; ok {
		params.Set("tf", window)
	}

	resp := &SearchResponse{Engine: e.Name()}
	if q.TimeRange != "" {
		if _, ok := braveWindow[q.TimeRange]; !ok {
			resp.note("Brave has no %q filter; the results are unfiltered by time", q.TimeRange)
		}
	}

	results, err := s.scrapeResults(scrapeRequest{
		Engine: e.Name(),
		URL:    "https://search.brave.com/search?" + params.Encode(),
		Wait:   ".snippet, #results, main",
		JS:     braveResultsJS,
		Want:   q.NumResults + 4,
	})
	if err != nil {
		return nil, err
	}
	resp.Results = results
	return finish(resp, q), nil
}

// braveWindow maps a time range onto Brave's tf parameter.
var braveWindow = map[string]string{
	"past_day":   "pd",
	"past_week":  "pw",
	"past_month": "pm",
	"past_year":  "py",
}

// braveResultsJS reads Brave's result cards.
//
// It keys off data-type and the semantic class names, not the layout classes:
// Brave's page is built with Svelte, so most of its classes carry a build hash
// (`result-body svelte-1rq4ngz`) and change on every deploy. What does not
// change is `.snippet[data-type="web"]` marking a web result, `.title` holding
// the title and `cite` holding the display URL - those are there because the
// page's own code and its accessibility need them.
//
// The description has no stable hook at all, so it is recovered from the card's
// text by dropping the lines that are already accounted for. That is uglier
// than a selector and it survives a redeploy, which a selector does not.
const braveResultsJS = `(want) => {
  const out = [];
  const seen = new Set();

  // Web results specifically, and only if there are none does this fall back
  // to every snippet on the page. The two must not be one comma-separated
  // query: that returns document order, and the AI answer card sits above the
  // results and would be reported as the first hit - with the model's summary
  // where the page title belongs.
  let cards = document.querySelectorAll('.snippet[data-type="web"]');
  if (cards.length === 0) cards = document.querySelectorAll('#results .snippet, .snippet');

  for (const card of cards) {
    if (out.length >= want) break;
    if (card.getAttribute('data-type') && card.getAttribute('data-type') !== 'web') continue;
    const link = card.querySelector('a[href^="http"]');
    if (!link) continue;

    // The title has to come from the title element. Falling back to the
    // link's text picks up the site name and the breadcrumb URL that sit
    // above it inside the same anchor.
    const titleEl = card.querySelector('.title, .snippet-title, h2, h3');
    if (!titleEl) continue;
    const title = (titleEl.innerText || '').trim().split('\n').pop().trim();
    if (!title || seen.has(link.href)) continue;
    seen.add(link.href);

    const citeEl = card.querySelector('cite.snippet-url, cite, .netloc');
    const displayUrl = citeEl ? (citeEl.innerText || '').trim() : '';

    // The description is whatever is left of the card once the site name, the
    // display URL and the title are taken out. The longest remaining line is
    // it: the others are badges and dates.
    let snippet = '';
    const known = new Set([title, displayUrl].filter(Boolean));
    for (const line of (card.innerText || '').split('\n').map(s => s.trim())) {
      if (!line || known.has(line) || line.length < 40) continue;
      if (line.length > snippet.length) snippet = line;
    }

    out.push({ title: title, url: link.href, snippet: snippet, display_url: displayUrl });
  }
  return out;
}`

// ---------------------------------------------------------------------------
// Startpage
// ---------------------------------------------------------------------------

// startpageEngine scrapes startpage.com, which serves Google's results without
// Google's tracking. That makes it the closest thing here to a second opinion
// on Google itself: when the google engine is rate-limited, this often returns
// the same result set - and it returns real destination URLs rather than the
// opaque redirects Google's own page carries.
type startpageEngine struct{}

func (startpageEngine) Name() string { return EngineStartpage }

func (startpageEngine) Description() string {
	return "Startpage, which serves Google's results without the tracking. The closest " +
		"substitute when the google engine is rate-limited, and its result URLs are " +
		"already unwrapped. Needs the browser."
}

func (startpageEngine) NeedsBrowser() bool { return true }

func (e startpageEngine) Search(_ context.Context, s *Scraper, q SearchQuery) (*SearchResponse, error) {
	params := url.Values{}
	params.Set("query", q.Text())
	params.Set("language", "english")
	if q.Page > 1 {
		params.Set("page", strconv.Itoa(q.Page))
	}
	if q.Region != "" {
		params.Set("lui", strings.ToLower(q.Region))
	}
	if window, ok := startpageWindow[q.TimeRange]; ok {
		params.Set("with_date", window)
	}

	resp := &SearchResponse{Engine: e.Name()}
	if q.TimeRange != "" {
		if _, ok := startpageWindow[q.TimeRange]; !ok {
			resp.note("Startpage has no %q filter; the results are unfiltered by time", q.TimeRange)
		}
	}

	results, err := s.scrapeResults(scrapeRequest{
		Engine: e.Name(),
		URL:    "https://www.startpage.com/sp/search?" + params.Encode(),
		Wait:   ".result, .w-gl, #main",
		JS:     startpageResultsJS,
		Want:   q.NumResults + 4,
	})
	if err != nil {
		return nil, err
	}
	resp.Results = results
	return finish(resp, q), nil
}

// startpageWindow maps a time range onto Startpage's with_date parameter.
var startpageWindow = map[string]string{
	"past_day":   "d",
	"past_week":  "w",
	"past_month": "m",
	"past_year":  "y",
}

// startpageResultsJS reads Startpage's result list.
//
// Startpage's classes are emotion-generated and carry a hash too, but each one
// is paired with a semantic name - `wgl-title css-i3irj7`, `description
// css-1507v2l` - and it is the semantic half that is matched here.
const startpageResultsJS = `(want) => {
  const out = [];
  const seen = new Set();
  const cards = document.querySelectorAll('.result, .w-gl__result, [data-testid="web-result"]');

  for (const card of cards) {
    if (out.length >= want) break;
    const link = card.querySelector('a[href^="http"]');
    if (!link) continue;

    const titleEl = card.querySelector('.wgl-title, h2, h3');
    const title = ((titleEl ? titleEl.innerText : link.innerText) || '').trim().split('\n')[0];
    if (!title || seen.has(link.href)) continue;
    seen.add(link.href);

    const snippetEl = card.querySelector('.description, .w-gl__description, p');
    const citeEl = card.querySelector('.w-gl__result-url, cite, .link-text');

    out.push({
      title: title,
      url: link.href,
      snippet: snippetEl ? (snippetEl.innerText || '').trim() : '',
      display_url: citeEl ? (citeEl.innerText || '').trim() : '',
    });
  }
  return out;
}`

// ---------------------------------------------------------------------------
// The shared browser scrape
// ---------------------------------------------------------------------------

// scrapeRequest is one browser-driven result page.
type scrapeRequest struct {
	Engine string
	URL    string
	Wait   string
	JS     string
	Want   int
}

// scrapeResults loads a results page and runs its extraction script.
//
// A wait that times out is not treated as fatal. The wait is a way to avoid
// extracting from a half-built page, not a test of whether the page is usable:
// these engines render progressively, and a selector that has stopped matching
// would otherwise turn a page full of results into an error. So the extraction
// runs regardless, and only an extraction that finds nothing is reported - with
// the reason distinguished, because "you have been challenged" and "this
// server's selectors are stale" call for completely different responses and
// look identical if both are reported as "no results".
func (s *Scraper) scrapeResults(req scrapeRequest) ([]SearchResult, error) {
	page, err := s.Browser.Acquire()
	if err != nil {
		return nil, err
	}
	defer page.Release()

	if err := page.Open(req.URL); err != nil {
		return nil, err
	}
	waited := page.WaitForResults(req.Wait) == nil
	if !waited {
		// The page may still be arriving, so give it the benefit of the doubt
		// once before deciding there is nothing there.
		page.Settle(2500)
	}

	raw, err := page.Evaluate(req.JS, req.Want)
	if err != nil {
		return nil, fmt.Errorf("reading the results: %w", err)
	}
	results := decodeResults(raw)
	if len(results) > 0 {
		return results, nil
	}

	if page.Blocked() {
		return nil, ErrBlocked
	}
	if !waited {
		return nil, fmt.Errorf("%s returned a page with no results on it. This engine is strict "+
			"about headless browsers, and that is the usual cause rather than a fault: %s answers "+
			"the same query with no browser at all, and running this server with --headed gets %s "+
			"itself working", req.Engine, EngineDuckDuckGo, req.Engine)
	}
	return nil, nil
}
