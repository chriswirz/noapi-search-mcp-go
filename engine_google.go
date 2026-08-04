package main

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"
)

// googleEngine scrapes google.com/search through the headless browser.
//
// It has to be the browser: Google now refuses a plain HTTP client outright,
// answering a "please enable JavaScript" page rather than results. And the
// result links on the rendered page are opaque /goto? trackers, so the real
// destinations are recovered afterwards by asking Google where each one goes.
// Both of those are why this engine is the slowest one here and why the others
// are worth having.
type googleEngine struct{}

func (googleEngine) Name() string { return EngineGoogle }

func (googleEngine) Description() string {
	return "Google. The most complete results and the only engine the vertical " +
		"searches use, but it rate-limits scrapers and needs the browser."
}

func (googleEngine) NeedsBrowser() bool { return true }

func (e googleEngine) Search(ctx context.Context, s *Scraper, q SearchQuery) (*SearchResponse, error) {
	page, err := s.Browser.Acquire()
	if err != nil {
		return nil, err
	}
	defer page.Release()

	if err := page.Open(googleSearchURL(q)); err != nil {
		return nil, err
	}
	// Waiting for the results container rather than a fixed delay: it is the
	// one thing on the page that means "the results are here".
	if err := page.WaitForResults("div#search"); err != nil {
		if page.Blocked() {
			return nil, ErrBlocked
		}
		return nil, err
	}

	raw, err := page.Evaluate(googleResultsJS, q.NumResults+8)
	if err != nil {
		return nil, fmt.Errorf("reading the results: %w", err)
	}
	results := decodeResults(raw)

	resp := &SearchResponse{Engine: e.Name(), Results: results}
	if len(results) == 0 && page.Blocked() {
		return nil, ErrBlocked
	}
	s.UnwrapAll(ctx, resp.Results)
	return finish(resp, q), nil
}

// googleSearchURL builds the search URL. num is asked for a few above what the
// caller wants because the page mixes ads and "people also ask" blocks in with
// the results, and the extraction drops those.
func googleSearchURL(q SearchQuery) string {
	params := url.Values{}
	params.Set("q", q.Text())
	params.Set("num", strconv.Itoa(q.NumResults+8))
	if q.Language != "" {
		params.Set("hl", q.Language)
		params.Set("lr", "lang_"+q.Language)
	} else {
		params.Set("hl", "en")
	}
	if q.Region != "" {
		params.Set("gl", q.Region)
	}
	if offset := q.Offset(); offset > 0 {
		params.Set("start", strconv.Itoa(offset))
	}
	if tbs, ok := googleTBS[q.TimeRange]; ok {
		params.Set("tbs", tbs)
	}
	if q.SafeSearch {
		params.Set("safe", "active")
	}
	return "https://www.google.com/search?" + params.Encode()
}

// googleResultsJS reads the organic results off a rendered results page.
//
// It is written against the layout rather than against a class name, because
// Google's class names are generated and change without notice - div.g, which
// every scraper including the Python server this one is modelled on keys off,
// matches nothing on the current page. What has stayed put is the shape: an
// <h3> inside the anchor that goes to the result, with the snippet somewhere
// in a shared ancestor. So this walks the headings and climbs from each one
// until it finds the block that holds its snippet.
const googleResultsJS = `(want) => {
  const out = [];
  const seen = new Set();
  const snippetSelector = 'div[data-sncf], div.VwiC3b, span.aCOpRe, div[style*="-webkit-line-clamp"]';

  for (const h3 of document.querySelectorAll('div#search h3')) {
    if (out.length >= want) break;
    const link = h3.closest('a[href]');
    if (!link) continue;

    const title = (h3.innerText || '').trim();
    const href = link.href || '';
    if (!title || !href || seen.has(href)) continue;

    // Climb until the block also contains the snippet. Six levels is enough
    // for every layout seen and stops this walking out to <body> on the ones
    // where there is no snippet at all.
    let box = link.parentElement;
    for (let i = 0; i < 6 && box; i++) {
      if (box.querySelector(snippetSelector)) break;
      box = box.parentElement;
    }
    const snippetEl = box ? box.querySelector(snippetSelector) : null;
    const citeEl = box ? box.querySelector('cite') : null;

    seen.add(href);
    out.push({
      title: title,
      url: href,
      snippet: snippetEl ? (snippetEl.innerText || '').trim() : '',
      display_url: citeEl ? (citeEl.innerText || '').trim() : '',
    });
  }
  return out;
}`

// googleVerticalURL builds a search URL for one of Google's tbm verticals:
// news, images, shopping and books are the same endpoint with a mode switch.
func googleVerticalURL(mode, query string, num int, extra url.Values) string {
	params := url.Values{}
	params.Set("q", query)
	params.Set("hl", "en")
	if mode != "" {
		params.Set("tbm", mode)
	}
	if num > 0 {
		params.Set("num", strconv.Itoa(num))
	}
	for key, values := range extra {
		for _, v := range values {
			params.Add(key, v)
		}
	}
	return "https://www.google.com/search?" + params.Encode()
}

// googleAnswerURL is a plain search whose point is the answer card at the top -
// the weather panel, the flight card - rather than the result list.
func googleAnswerURL(query string) string {
	return "https://www.google.com/search?q=" + url.QueryEscape(query) + "&hl=en"
}

// cleanText collapses the runs of blank lines that innerText leaves behind and
// trims the result. Every scraper here ends up doing it, because a model
// reading a wall of empty lines is paying for them.
func cleanText(s string) string {
	lines := strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, line := range lines {
		line = strings.TrimRight(line, " \t")
		if strings.TrimSpace(line) == "" {
			blank++
			if blank > 1 {
				continue
			}
			out = append(out, "")
			continue
		}
		blank = 0
		out = append(out, line)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// truncate cuts text to at most limit bytes, saying so when it does.
//
// Saying so is the point. Text that stops mid-sentence with no explanation
// reads, to a model, exactly like a page that ended there - and summarising
// half an article as though it were the whole one is a failure nothing
// downstream can detect.
//
// The cut is moved back to a rune boundary. Cutting mid-character would leave
// invalid UTF-8, which json.Marshal silently replaces with U+FFFD.
func truncate(s string, limit int) string {
	if limit <= 0 || len(s) <= limit {
		return s
	}
	cut := s[:limit]
	// A partial rune at the end decodes as RuneError with a width of one, so
	// trimming while that holds lands on the last whole character.
	for len(cut) > 0 {
		r, size := utf8.DecodeLastRuneInString(cut)
		if r != utf8.RuneError || size > 1 {
			break
		}
		cut = cut[:len(cut)-1]
	}
	return cut + fmt.Sprintf("\n\n... [truncated; showing the first %d characters of %d]",
		utf8.RuneCountInString(cut), utf8.RuneCountInString(s))
}
