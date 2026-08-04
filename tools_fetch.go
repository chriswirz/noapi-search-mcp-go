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
		Name:     "visit_page",
		Title:    "Read a page",
		Path:     "/fetch",
		ReadOnly: true,
		Summary:  "Fetch a URL and return its readable text.",
		Description: `Fetch a web page and return its text, with the navigation, adverts and
boilerplate stripped out.

This is the other half of searching. A search result gives you a title and two
lines of snippet, and answering from those is how a confident wrong answer gets
made: the snippet is chosen by the engine to match the query, not to represent
the page. When the answer depends on what a page actually says, read it.

The page is rendered in a real browser first, so this works on sites that build
their content in script - which a plain HTTP fetch returns an empty shell for.

Sample prompts:
  - "Read this and summarise it: https://..."
  - "What does that first result actually say?"
  - "Open the docs page and find the flag for it"

Long pages are truncated, and the result says so where it happens.`,
		Schema: objectSchema([]string{"url"}, map[string]any{
			"url": stringProp("The full URL to fetch, including the scheme."),
			"max_characters": intProp("Cap on the returned text. The server's own limit still applies.",
				0, 0, maxPageCharsCeiling),
			"include_links": boolProp("Also return the page's outbound links, which is how you "+
				"navigate onward from an index or a table of contents without guessing at URLs.", false),
		}),
		Run: func(_ context.Context, s *Server, raw json.RawMessage) (any, error) {
			var args struct {
				URL           string `json:"url"`
				MaxCharacters int    `json:"max_characters"`
				IncludeLinks  bool   `json:"include_links"`
			}
			if err := bind(raw, &args); err != nil {
				return nil, err
			}
			target, err := checkURL(args.URL)
			if err != nil {
				return nil, err
			}
			limit := s.cfg.Search.MaxPageChars
			if args.MaxCharacters > 0 && args.MaxCharacters < limit {
				limit = args.MaxCharacters
			}
			return s.scraper.fetchPage(target, limit, args.IncludeLinks)
		},
	})
}

// checkURL validates a URL argument. A bare hostname is upgraded to https
// rather than refused: it is what a person means, and refusing it costs a turn
// to add four characters.
func checkURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", badInput("url is required")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", badInput("url %q: %v", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", badInput("url: only http and https can be fetched, not %q", u.Scheme)
	}
	if u.Host == "" {
		return "", badInput("url %q has no host", raw)
	}
	return u.String(), nil
}

// PageText is one fetched page.
type PageText struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`

	// FinalURL is where the fetch ended up. It differs from URL after a
	// redirect, and a caller citing the page should cite this one.
	FinalURL string `json:"final_url,omitempty"`

	Text string `json:"text"`

	// Truncated says the text was cut, and Characters is how much of the page
	// was returned. A model that cannot tell truncation from the end of an
	// article will summarise half a page as though it were all of it.
	Truncated  bool `json:"truncated"`
	Characters int  `json:"characters"`

	Links []PageLink `json:"links,omitempty"`
	Notes []string   `json:"notes,omitempty"`
}

// PageLink is one outbound link.
type PageLink struct {
	Text string `json:"text"`
	URL  string `json:"url"`
}

func (p *PageText) Render() string {
	var b strings.Builder
	if p.Title != "" {
		fmt.Fprintf(&b, "%s\n", p.Title)
	}
	fmt.Fprintf(&b, "%s\n", p.FinalURL)
	if p.FinalURL != p.URL {
		fmt.Fprintf(&b, "(redirected from %s)\n", p.URL)
	}
	for _, note := range p.Notes {
		fmt.Fprintf(&b, "Note: %s\n", note)
	}
	fmt.Fprintf(&b, "\n%s\n", p.Text)
	if len(p.Links) > 0 {
		b.WriteString("\nLinks:\n")
		for _, link := range p.Links {
			fmt.Fprintf(&b, "  %s\n    %s\n", link.Text, link.URL)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// fetchPage renders a page and extracts its readable text.
func (s *Scraper) fetchPage(target string, limit int, includeLinks bool) (*PageText, error) {
	page, err := s.Browser.Acquire()
	if err != nil {
		return nil, err
	}
	defer page.Release()

	// Deliberately not Open: that dismisses Google's consent banner and checks
	// for a Google block, neither of which means anything on an arbitrary site.
	if _, err := page.Goto(target); err != nil {
		return nil, fmt.Errorf("fetching %s: %w", target, err)
	}
	// A moment for whatever the page loads after DOMContentLoaded. There is no
	// general signal for "this page has finished putting its content in", and
	// waiting for network idle hangs on anything holding a connection open.
	page.Settle(1500)

	var out struct {
		Title string     `json:"title"`
		Text  string     `json:"text"`
		Links []PageLink `json:"links"`
	}
	if err := page.EvaluateInto(&out, readableTextJS, includeLinks); err != nil {
		return nil, fmt.Errorf("reading %s: %w", target, err)
	}

	text := cleanText(out.Text)
	result := &PageText{
		URL:      target,
		FinalURL: page.URL(),
		Title:    strings.TrimSpace(out.Title),
		Links:    out.Links,
	}
	if text == "" {
		return nil, fmt.Errorf("no readable text at %s: the page may be a PDF, an image, "+
			"or a site that will not render without a sign-in", target)
	}
	if len(text) > limit {
		result.Truncated = true
		text = truncate(text, limit)
	}
	result.Text = text
	result.Characters = len(text)
	return result, nil
}

// readableTextJS strips the page down to its article text.
//
// It removes the furniture first and then looks for the content element,
// rather than the other way around: on a page with no <article> or <main>, the
// fallback is <body>, and a <body> that still contains the navigation and the
// cookie banner is most of what makes scraped text unreadable.
const readableTextJS = `(includeLinks) => {
  const out = { title: document.title || '', text: '', links: [] };

  // Collect the links before the furniture is removed, since the removal takes
  // out navigation elements that a table of contents legitimately lives in -
  // but only from the content area, once that is known.
  const junk = 'script, style, noscript, iframe, svg, canvas, ' +
    'nav, header, footer, aside, form, ' +
    '[role="navigation"], [role="banner"], [role="complementary"], ' +
    '[aria-hidden="true"], .sidebar, .ad, .ads, .advertisement, ' +
    '.cookie-banner, .newsletter, .related-posts, .comments';

  const article = document.querySelector(
    'article, main, [role="main"], .post-content, .article-body, ' +
    '.entry-content, #content, .content'
  );
  const source = article || document.body;
  if (!source) return out;

  // The removal happens on a copy: mutating the live document breaks a page
  // that is still rendering, and this may be asked to read it again.
  const copy = source.cloneNode(true);
  copy.querySelectorAll(junk).forEach(el => el.remove());

  // The copy is put back into the page, off to one side, before its text is
  // read. innerText is the layout-aware reading - it is what puts a line
  // break between two blocks and a space between two inline elements - and a
  // detached node has no layout, so innerText there quietly degrades to
  // textContent and hands back every word run together with the tags simply
  // gone. Off-screen rather than display:none, which would take the layout
  // away again and bring the same problem back.
  const holder = document.createElement('div');
  holder.setAttribute('aria-hidden', 'true');
  holder.style.cssText =
    'position:absolute!important;left:-99999px!important;top:0!important;' +
    'width:1024px!important;height:auto!important;overflow:hidden!important;' +
    'opacity:0!important;pointer-events:none!important';
  holder.appendChild(copy);
  document.body.appendChild(holder);
  try {
    out.text = copy.innerText || copy.textContent || '';
  } finally {
    holder.remove();
  }

  if (includeLinks) {
    const seen = new Set();
    for (const a of source.querySelectorAll('a[href]')) {
      if (out.links.length >= 100) break;
      const href = a.href;
      const text = (a.innerText || '').trim().replace(/\s+/g, ' ');
      if (!href || !href.startsWith('http') || !text || seen.has(href)) continue;
      seen.add(href);
      out.links.push({ text: text.slice(0, 120), url: href });
    }
  }
  return out;
}`
