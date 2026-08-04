package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

func init() {
	registerOperation(&Operation{
		Name:     "web_search",
		Title:    "Web search",
		Path:     "/search",
		ReadOnly: true,
		Summary:  "Search the web through any of several engines.",
		Description: `Search the web and get back titles, URLs and snippets.

This is the general search tool. Pick the engine to suit the situation:
  - google (default) returns the fullest results, and is the one that gets
    rate-limited. A call that comes back blocked is not broken; try another
    engine.
  - duckduckgo, bing and mojeek need no browser at all, so they are fast and
    they keep working when google will not.
  - brave and mojeek run their own crawlers, and startpage serves Google's
    results without the tracking. When an answer looks wrong, asking a second
    engine is the cheapest way to find out.

Sample prompts:
  - "Search for the best Go web frameworks"
  - "Find Reddit discussions about home labs from the past week"
  - "Search arxiv.org for retrieval augmented generation"
  - "Look that up on DuckDuckGo instead"

The snippet is not the page. When the answer depends on what a result actually
says, follow it with visit_page rather than answering from the snippet.`,
		Schema: objectSchema([]string{"query"}, searchProps(true)),
		Run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var args searchArgs
			if err := bind(raw, &args); err != nil {
				return nil, err
			}
			q, err := args.query(s.cfg.Search)
			if err != nil {
				return nil, err
			}
			name := args.Engine
			if name == "" {
				name = s.cfg.Search.Engine
			}
			engine, err := LookupEngine(name)
			if err != nil {
				return nil, &InputError{msg: err.Error()}
			}
			return s.scraper.Run(ctx, engine, q)
		},
	})

	registerOperation(&Operation{
		Name:     "google_search",
		Title:    "Google search",
		Path:     "/search/google",
		ReadOnly: true,
		Summary:  "Search Google specifically.",
		Description: `Search Google and return titles, URLs and snippets.

This is web_search pinned to Google, kept because it is the name the tool has
in the Python server this one is modelled on and in most existing client
configurations. New callers should prefer web_search, which takes an engine
argument and can fall back when Google rate-limits this address.

Sample prompts:
  - "Google the release date for Go 1.25"
  - "Search Google for stackoverflow answers about context cancellation"`,
		Schema: objectSchema([]string{"query"}, searchProps(false)),
		Run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var args searchArgs
			if err := bind(raw, &args); err != nil {
				return nil, err
			}
			q, err := args.query(s.cfg.Search)
			if err != nil {
				return nil, err
			}
			return s.scraper.Run(ctx, googleEngine{}, q)
		},
	})

	registerVertical("google_news", "/search/news", "Google News",
		"Search Google News for recent headlines.",
		`Search Google News and return headlines with their source, age and snippet.

Sample prompts:
  - "What is the latest news about the CHIPS act?"
  - "Any news on the Ariane 6 launch this week?"
  - "Top headlines about the semiconductor industry"`,
		"nws", newsResultsJS)

	registerVertical("google_scholar", "/search/scholar", "Google Scholar",
		"Search Google Scholar for academic papers.",
		`Search Google Scholar and return papers with authors, venue and citation counts.

Sample prompts:
  - "Find papers on transformer attention mechanisms"
  - "What does the research say about intermittent fasting?"
  - "Look up citations for the original ResNet paper"`,
		"scholar", scholarResultsJS)

	registerVertical("google_books", "/search/books", "Google Books",
		"Search Google Books for books and publications.",
		`Search Google Books and return titles with authors, ISBNs where they are shown, and snippets.

Sample prompts:
  - "Find books about distributed systems"
  - "Search for books by Ursula Le Guin"
  - "What textbooks cover measure theory?"`,
		"bks", booksResultsJS)

	registerVertical("google_shopping", "/search/shopping", "Google Shopping",
		"Search Google Shopping for products and prices.",
		`Search Google Shopping and return products with prices, stores and ratings.

Prices are whatever the page showed at the time, in whatever currency the page
chose, and they are frequently stale or exclude shipping. Treat them as a
starting point for a comparison rather than as quotes.

Sample prompts:
  - "How much does a Framework 13 cost?"
  - "Compare prices for Sony WH-1000XM5"
  - "Find mechanical keyboards under $100"`,
		"shop", shoppingResultsJS)

	registerOperation(&Operation{
		Name:     "google_images",
		Title:    "Google Images",
		Path:     "/search/images",
		ReadOnly: true,
		Summary:  "Search Google Images and return image URLs.",
		Description: `Search Google Images and return the image URLs with the pages they came from.

This returns URLs rather than the image bytes: a handful of full-size images is
several megabytes, and a tool that spends a context window on pictures nobody
asked to see is a bad trade. Fetch the ones that matter.

Sample prompts:
  - "Find pictures of the Antikythera mechanism"
  - "Show me images of brutalist housing estates"
  - "What does a DGX Spark look like?"`,
		Schema: objectSchema([]string{"query"}, searchProps(false)),
		Run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var args searchArgs
			if err := bind(raw, &args); err != nil {
				return nil, err
			}
			q, err := args.query(s.cfg.Search)
			if err != nil {
				return nil, err
			}
			return s.scraper.imageSearch(q)
		},
	})
}

// registerVertical installs one of Google's tbm-mode searches. The four of
// them differ only in the mode switch and the extraction script, so writing
// four near-identical operations by hand would be four places for a fix to be
// applied to three of.
func registerVertical(name, path, title, summary, description, mode, script string) {
	registerOperation(&Operation{
		Name:        name,
		Title:       title,
		Path:        path,
		ReadOnly:    true,
		Summary:     summary,
		Description: description,
		Schema: objectSchema([]string{"query"}, map[string]any{
			"query":       stringProp("What to search for."),
			"num_results": intProp("How many results to return.", 5, 1, 50),
		}),
		Run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var args struct {
				Query      string `json:"query"`
				NumResults int    `json:"num_results"`
			}
			if err := bind(raw, &args); err != nil {
				return nil, err
			}
			if strings.TrimSpace(args.Query) == "" {
				return nil, badInput("query is required")
			}
			return s.scraper.vertical(ctx, verticalRequest{
				Tool:  name,
				Mode:  mode,
				Query: strings.TrimSpace(args.Query),
				Want:  clampResults(args.NumResults, 5, s.cfg.Search.MaxResults),
				JS:    script,
			})
		},
	})
}

// Run executes one search and normalises how failure is reported. Every engine
// can be blocked, and every engine's block should read the same way to a model
// deciding what to do about it.
func (s *Scraper) Run(ctx context.Context, engine Engine, q SearchQuery) (*SearchResponse, error) {
	resp, err := engine.Search(ctx, s, q)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", engine.Name(), err)
	}
	if len(resp.Results) == 0 {
		resp.note("no results; the query may be too narrow, or this engine may have " +
			"answered with something other than a result list. Trying another engine is cheap.")
	}
	return resp, nil
}

// Render presents a search response as the text a model reads.
func (r *SearchResponse) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d results for %q from %s", r.Count, r.Query, r.Engine)
	if r.Page > 1 {
		fmt.Fprintf(&b, " (page %d)", r.Page)
	}
	b.WriteString("\n")
	for _, note := range r.Notes {
		fmt.Fprintf(&b, "Note: %s\n", note)
	}
	for _, result := range r.Results {
		fmt.Fprintf(&b, "\n%d. %s\n   %s\n", result.Rank, result.Title, result.URL)
		if result.Redirected {
			b.WriteString("   (this is the engine's redirect; the destination could not be resolved)\n")
		}
		if result.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", result.Snippet)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// ---------------------------------------------------------------------------
// The Google verticals
// ---------------------------------------------------------------------------

// verticalRequest is one tbm-mode search.
type verticalRequest struct {
	Tool  string
	Mode  string
	Query string
	Want  int
	JS    string
}

// VerticalResult is one hit from a vertical search. The fields are a union
// across the four verticals; each one fills what it has, and a field left empty
// means the page did not show it.
type VerticalResult struct {
	Rank    int    `json:"rank"`
	Title   string `json:"title"`
	URL     string `json:"url,omitempty"`
	Snippet string `json:"snippet,omitempty"`

	Source  string `json:"source,omitempty"`  // news: the publication
	Age     string `json:"age,omitempty"`     // news: "3 hours ago"
	Authors string `json:"authors,omitempty"` // scholar and books
	CitedBy string `json:"cited_by,omitempty"`
	ISBN    string `json:"isbn,omitempty"`
	Price   string `json:"price,omitempty"` // shopping
	Store   string `json:"store,omitempty"`
	Rating  string `json:"rating,omitempty"`

	// Image is the thumbnail URL where the page carried one.
	Image string `json:"image,omitempty"`
}

// VerticalResponse is a whole vertical answer.
type VerticalResponse struct {
	Tool    string           `json:"tool"`
	Query   string           `json:"query"`
	Count   int              `json:"count"`
	Results []VerticalResult `json:"results"`
	Notes   []string         `json:"notes,omitempty"`
}

// Render presents a vertical response as text, printing only the fields the
// engine actually filled.
func (r *VerticalResponse) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d results for %q from %s\n", r.Count, r.Query, r.Tool)
	for _, note := range r.Notes {
		fmt.Fprintf(&b, "Note: %s\n", note)
	}
	for _, result := range r.Results {
		fmt.Fprintf(&b, "\n%d. %s\n", result.Rank, result.Title)
		for _, field := range []struct{ label, value string }{
			{"Source", joinNonEmpty(" - ", result.Source, result.Age)},
			{"Authors", result.Authors},
			{"Price", joinNonEmpty(" at ", result.Price, result.Store)},
			{"Rating", result.Rating},
			{"ISBN", result.ISBN},
			{"Cited by", result.CitedBy},
			{"URL", result.URL},
		} {
			if field.value != "" {
				fmt.Fprintf(&b, "   %s: %s\n", field.label, field.value)
			}
		}
		if result.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", result.Snippet)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// joinNonEmpty joins the parts that are set, so a result missing half its
// metadata does not render as "Source:  - ".
func joinNonEmpty(sep string, parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			kept = append(kept, strings.TrimSpace(p))
		}
	}
	return strings.Join(kept, sep)
}

// vertical runs one tbm-mode search in the browser.
func (s *Scraper) vertical(ctx context.Context, req verticalRequest) (*VerticalResponse, error) {
	page, err := s.Browser.Acquire()
	if err != nil {
		return nil, err
	}
	defer page.Release()

	target := googleVerticalURL(req.Mode, req.Query, req.Want+5, nil)
	if req.Mode == "scholar" {
		// Scholar is a different host with its own parameters, not a tbm mode.
		target = "https://scholar.google.com/scholar?hl=en&q=" + url.QueryEscape(req.Query)
	}
	if err := page.Open(target); err != nil {
		return nil, err
	}
	// The verticals render progressively, and there is no single element that
	// means "done" across all four, so this waits for the results container
	// and then gives the rest of the card a moment to arrive.
	if err := page.WaitForResults("div#search, #gs_res_ccl, div#rso, #main"); err != nil && page.Blocked() {
		return nil, ErrBlocked
	}
	page.Settle(1500)

	var results []VerticalResult
	if err := page.EvaluateInto(&results, req.JS, req.Want+5); err != nil {
		if page.Blocked() {
			return nil, ErrBlocked
		}
		return nil, fmt.Errorf("reading the results: %w", err)
	}

	resp := &VerticalResponse{Tool: req.Tool, Query: req.Query}
	for _, r := range results {
		if strings.TrimSpace(r.Title) == "" {
			continue
		}
		r.Snippet = cleanText(r.Snippet)
		r.Rank = len(resp.Results) + 1
		resp.Results = append(resp.Results, r)
		if len(resp.Results) >= req.Want {
			break
		}
	}
	resp.Count = len(resp.Results)
	if resp.Count == 0 {
		if page.Blocked() {
			return nil, ErrBlocked
		}
		resp.Notes = append(resp.Notes, "no results; this vertical shows nothing for some queries, "+
			"and web_search over the same words often does better")
	}
	// News and shopping links come through the same opaque /goto? tracker the
	// web results use, so they need the same unwrapping. Scholar and books
	// link straight out and pass through untouched.
	s.unwrapVertical(ctx, resp.Results)
	return resp, nil
}

// unwrapVertical resolves the tracker URLs on a page of vertical results. It
// borrows the search-result unwrapper rather than repeating its concurrency,
// which is the whole reason SearchResult is the shape the unwrapper takes.
func (s *Scraper) unwrapVertical(ctx context.Context, results []VerticalResult) {
	if !s.Search.ResolvesRedirects() {
		return
	}
	links := make([]SearchResult, 0, len(results))
	index := make([]int, 0, len(results))
	for i, r := range results {
		if isTracker(r.URL) {
			links = append(links, SearchResult{URL: r.URL})
			index = append(index, i)
		}
	}
	if len(links) == 0 {
		return
	}
	s.UnwrapAll(ctx, links)
	for at, link := range links {
		results[index[at]].URL = link.URL
	}
}

// imageSearch reads Google Images. It is separate from the other verticals
// because the image grid carries its data in link parameters rather than in
// text, so the extraction has a different shape.
func (s *Scraper) imageSearch(q SearchQuery) (*VerticalResponse, error) {
	page, err := s.Browser.Acquire()
	if err != nil {
		return nil, err
	}
	defer page.Release()

	if err := page.Open(googleVerticalURL("isch", q.Text(), 0, nil)); err != nil {
		return nil, err
	}
	// The grid is lazily built by script, so there is nothing to wait for
	// except the script having run.
	page.Settle(2500)

	var results []VerticalResult
	if err := page.EvaluateInto(&results, imageResultsJS, q.NumResults+5); err != nil {
		if page.Blocked() {
			return nil, ErrBlocked
		}
		return nil, fmt.Errorf("reading the results: %w", err)
	}

	resp := &VerticalResponse{Tool: "google_images", Query: q.Query}
	for _, r := range results {
		if r.Image == "" {
			continue
		}
		if r.Title == "" {
			r.Title = "(untitled image)"
		}
		r.Rank = len(resp.Results) + 1
		resp.Results = append(resp.Results, r)
		if len(resp.Results) >= q.NumResults {
			break
		}
	}
	resp.Count = len(resp.Results)
	if resp.Count == 0 {
		if page.Blocked() {
			return nil, ErrBlocked
		}
		resp.Notes = append(resp.Notes, "no images found")
	}
	return resp, nil
}

// ---------------------------------------------------------------------------
// Extraction scripts
// ---------------------------------------------------------------------------

// newsResultsJS reads the Google News tab. The cards carry a role="heading"
// element rather than an h3, which is the one reliable difference from the web
// results, and the source and age sit in a metadata line under it.
// newsResultsJS reads the Google News tab.
//
// The cards are found by data-news-cluster-id, which is there for Google's own
// code rather than for its appearance and so outlasts the class names. Their
// text is unlabelled but ordered - publication, headline, summary, age - so the
// fields are taken from the lines rather than from selectors, and the ones that
// can be recognised on sight (the age reads "2 hours ago") are matched instead
// of being taken by position.
const newsResultsJS = `(want) => {
  const out = [];
  const seen = new Set();
  let cards = document.querySelectorAll('[data-news-cluster-id]');
  if (cards.length === 0) cards = document.querySelectorAll('div#search .SoaBEf, div#rso .SoaBEf');

  for (const card of cards) {
    if (out.length >= want) break;
    const link = card.querySelector('a[href]');
    if (!link) continue;
    const heading = card.querySelector('.n0jPhd, div[role="heading"], h3');
    if (!heading) continue;

    const title = (heading.innerText || '').trim();
    const href = link.href;
    if (!title || !href || seen.has(href)) continue;
    seen.add(href);

    const lines = (card.innerText || '').split('\n')
      .map(s => s.trim())
      .filter(s => s && s !== '.' && s !== title);

    let age = '';
    for (const line of lines) {
      if (/^\d+\s*(minute|hour|day|week|month|year)s?\s+ago$/i.test(line) ||
          /^(yesterday|today)$/i.test(line)) {
        age = line;
        break;
      }
    }
    // The publication is the line above the headline, and the summary is the
    // longest line that is neither of those.
    const source = lines.length > 0 && lines[0] !== age ? lines[0] : '';
    let snippet = '';
    for (const line of lines) {
      if (line === source || line === age) continue;
      if (line.length > snippet.length) snippet = line;
    }
    const img = card.querySelector('img[src^="http"]');

    out.push({
      title: title,
      url: href,
      source: source,
      age: age,
      snippet: snippet,
      image: img ? img.src : '',
    });
  }
  return out;
}`

// scholarResultsJS reads Google Scholar, whose gs_ class names have been in
// place for a decade but are not all present on every layout: the three-class
// .gs_r.gs_or.gs_scl selector misses the rows Scholar serves when it drops
// gs_or, which is why the entry selectors here widen in steps and stop at the
// first one that matches anything.
const scholarResultsJS = `(want) => {
  const out = [];
  let entries = [];
  for (const sel of ['.gs_r.gs_or.gs_scl', '.gs_r.gs_scl', '#gs_res_ccl_mid .gs_r', '#gs_res_ccl .gs_r', '.gs_ri', '[data-cid]']) {
    entries = Array.from(document.querySelectorAll(sel)).filter(e => e.querySelector('.gs_rt, h3'));
    if (entries.length) break;
  }
  for (const entry of entries) {
    if (out.length >= want) break;
    const titleEl = entry.querySelector('.gs_rt') || entry.querySelector('h3');
    if (!titleEl) continue;
    const link = titleEl.querySelector('a');
    const authors = entry.querySelector('.gs_a');
    const snippet = entry.querySelector('.gs_rs');

    let citedBy = '';
    for (const a of entry.querySelectorAll('.gs_fl a')) {
      if ((a.textContent || '').includes('Cited by')) { citedBy = a.textContent.trim(); break; }
    }
    out.push({
      title: (titleEl.innerText || '').trim(),
      url: link ? link.href : '',
      authors: authors ? (authors.innerText || '').trim() : '',
      snippet: snippet ? (snippet.innerText || '').trim() : '',
      cited_by: citedBy,
    });
  }
  return out;
}`

// booksResultsJS reads the Books tab, digging an ISBN out of the card text
// when one is shown. The ISBN is worth the effort: it is the one identifier
// that makes a book result actionable somewhere else.
const booksResultsJS = `(want) => {
  const out = [];
  const seen = new Set();
  for (const h3 of document.querySelectorAll('div#search h3, div#rso h3')) {
    if (out.length >= want) break;
    const title = (h3.innerText || '').trim();
    if (!title || title.length < 3 || seen.has(title)) continue;
    if (title === 'Search Results' || title === 'Filters and topics') continue;

    let box = h3.parentElement;
    for (let i = 0; i < 5 && box; i++) {
      if (box.querySelector('a[href^="http"]') && (box.innerText || '').length > title.length + 20) break;
      box = box.parentElement;
    }
    if (!box) continue;
    seen.add(title);

    const link = box.querySelector('a[href*="books.google"], a[href^="http"]');
    const snippet = box.querySelector('.VwiC3b, .cmlJmd, [data-sncf]');

    // The author line is the metadata line that names a year or separates
    // names with commas, which is what distinguishes it from the snippet.
    let authors = '';
    for (const el of box.querySelectorAll('span, cite, div')) {
      const t = (el.innerText || '').trim();
      if (!t || t === title || t.includes('http') || t.length > 200) continue;
      if (/\b(19|20)\d{2}\b/.test(t) || t.includes(' - ')) { authors = t; break; }
    }

    // ISBN-13 starts 978 or 979; hyphens are stripped before the length check.
    let isbn = '';
    const text = (box.innerText || '') + ' ' + (link ? link.href : '');
    const m13 = text.match(/97[89][\d-]{10,16}/);
    if (m13) {
      const digits = m13[0].replace(/-/g, '');
      if (digits.length === 13) isbn = digits;
    }
    out.push({
      title: title,
      url: link ? link.href : '',
      authors: authors,
      snippet: snippet ? (snippet.innerText || '').trim() : '',
      isbn: isbn,
    });
  }
  return out;
}`

// shoppingResultsJS reads the Shopping tab. The product link is buried under
// one of several redirect wrappers, so this tries them in order of how much
// they can be trusted: the merchant URL Google itself records, then the
// destination inside a redirect, then any external link on the card.
const shoppingResultsJS = `(want) => {
  const out = [];
  const cards = document.querySelectorAll(
    '.sh-dgr__content, .sh-dlr__list-result, .sh-pr__product-result, [data-docid]'
  );
  for (const card of cards) {
    if (out.length >= want) break;
    const titleEl = card.querySelector('h3, h4, .tAxDx, .Xjkr3b, .EI11Pd');
    const title = titleEl ? (titleEl.innerText || '').trim() : '';
    if (!title) continue;

    const priceEl = card.querySelector('.a8Pemb, .HRLxBb, .kHxwFf, .T14wmb');
    const storeEl = card.querySelector('.aULzUe, .IuHnof, .E5ocAb, .dD8iuc');
    const ratingEl = card.querySelector('.Rsc7Yb, .QIrs8, .yi40Hd');

    let url = '';
    const merchant = card.querySelector('a[data-merchant-url]');
    if (merchant) url = merchant.getAttribute('data-merchant-url') || '';
    if (!url) {
      for (const a of card.querySelectorAll('a[href]')) {
        try {
          const u = new URL(a.href, location.href);
          const target = u.searchParams.get('adurl') || u.searchParams.get('q') || u.searchParams.get('url');
          if (target && target.startsWith('http')) { url = target; break; }
          if (u.hostname && !u.hostname.endsWith('google.com')) { url = a.href; break; }
        } catch (e) { /* a malformed href is not a result */ }
      }
    }
    const img = card.querySelector('img[src^="http"]');
    out.push({
      title: title,
      url: url,
      price: priceEl ? (priceEl.innerText || '').trim() : '',
      store: storeEl ? (storeEl.innerText || '').trim() : '',
      rating: ratingEl ? (ratingEl.innerText || '').trim() : '',
      image: img ? img.src : '',
    });
  }
  return out;
}`

// imageResultsJS reads the image grid. The full-size URL is a parameter of the
// /imgres link behind each thumbnail; where that is missing the thumbnail is
// reported as both, which is honest and still useful.
const imageResultsJS = `(want) => {
  const out = [];
  const seen = new Set();
  for (const a of document.querySelectorAll('a[href*="/imgres"], div[data-id] a[jsname]')) {
    if (out.length >= want) break;
    const img = a.querySelector('img[src^="http"], img[data-src^="http"]');
    if (!img) continue;
    const thumb = img.src || img.getAttribute('data-src') || '';
    if (!thumb || thumb.startsWith('data:') || seen.has(thumb)) continue;
    seen.add(thumb);

    let full = '', source = '';
    try {
      const u = new URL(a.href, location.href);
      full = u.searchParams.get('imgurl') || '';
      source = u.searchParams.get('imgrefurl') || '';
    } catch (e) { /* not every anchor is an imgres link */ }

    out.push({
      title: img.alt || '',
      image: full || thumb,
      url: source,
      snippet: full && full !== thumb ? 'thumbnail: ' + thumb : '',
    });
  }
  if (out.length === 0) {
    for (const img of document.querySelectorAll('#search img[src^="http"], #islrg img[src^="http"]')) {
      if (out.length >= want) break;
      if (img.naturalWidth < 50 || img.naturalHeight < 50) continue;
      out.push({ title: img.alt || '', image: img.src, url: '' });
    }
  }
  return out;
}`
