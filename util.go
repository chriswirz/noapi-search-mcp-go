package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/playwright-community/playwright-go"
	"golang.org/x/net/html"
)

// readAllLimited reads at most limit bytes, so a page that turns out to be a
// gigabyte cannot take the process down with it.
func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit))
}

// WaitForResults waits for the element that means the results have arrived.
// Every scraper needs this and every one of them wants the same treatment of a
// timeout: it is not necessarily a failure, because the page may have rendered
// something else useful, so the caller decides.
func (p *Page) WaitForResults(selector string) error {
	if err := p.Locator(selector).First().WaitFor(playwright.LocatorWaitForOptions{
		State:   playwright.WaitForSelectorStateAttached,
		Timeout: playwright.Float(15000),
	}); err != nil {
		return fmt.Errorf("the results never appeared (waiting for %s): %w", selector, err)
	}
	return nil
}

// decodeResults converts the value an extraction script returned into results.
// Playwright hands back `any` built from JSON, so this round-trips it rather
// than type-asserting a nested map by hand: the extraction scripts already
// produce exactly the field names SearchResult declares.
func decodeResults(raw any) []SearchResult {
	var out []SearchResult
	if !decodeJSON(raw, &out) {
		return nil
	}
	// Anything without a title and a URL is not a result: the scripts are
	// deliberately permissive so that a layout change degrades rather than
	// returning nothing, and this is where the debris is dropped.
	kept := out[:0]
	for _, r := range out {
		r.Title = strings.TrimSpace(r.Title)
		r.Snippet = cleanText(r.Snippet)
		if r.Title == "" || !strings.HasPrefix(r.URL, "http") {
			continue
		}
		kept = append(kept, r)
	}
	return kept
}

// decodeJSON re-decodes a value Playwright returned into a typed destination.
// It reports whether it worked; a false is a layout that no longer matches the
// script, which every caller handles as "no results" rather than as an error.
func decodeJSON(raw any, dest any) bool {
	if raw == nil {
		return false
	}
	encoded, err := json.Marshal(raw)
	if err != nil {
		return false
	}
	return json.Unmarshal(encoded, dest) == nil
}

// EvaluateInto runs a script in the page and decodes its result into dest.
func (p *Page) EvaluateInto(dest any, script string, args ...any) error {
	raw, err := p.Evaluate(script, args...)
	if err != nil {
		return err
	}
	if !decodeJSON(raw, dest) {
		return fmt.Errorf("the page returned nothing this server could read; the layout has probably changed")
	}
	return nil
}

// -- minimal HTML querying, for the engines that need no browser --------------

// nodeText is all the text under a node, with whitespace collapsed.
func nodeText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.TextNode {
			b.WriteString(n.Data)
			return
		}
		if n.Type == html.ElementNode && (n.Data == "script" || n.Data == "style") {
			return
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

// attr returns an element's attribute value.
func attr(n *html.Node, name string) string {
	for _, a := range n.Attr {
		if a.Key == name {
			return a.Val
		}
	}
	return ""
}

// hasClass reports whether a node carries a class, matched on whole words so
// that "result" does not match "result__snippet".
func hasClass(n *html.Node, class string) bool {
	for _, field := range strings.Fields(attr(n, "class")) {
		if field == class {
			return true
		}
	}
	return false
}

// findAll collects every element matching a predicate, in document order.
func findAll(n *html.Node, match func(*html.Node) bool) []*html.Node {
	var out []*html.Node
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode && match(n) {
			out = append(out, n)
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return out
}

// find returns the first element matching a predicate, or nil.
func find(n *html.Node, match func(*html.Node) bool) *html.Node {
	if all := findAll(n, match); len(all) > 0 {
		return all[0]
	}
	return nil
}

// byClass matches an element of the given tag carrying the given class. An
// empty tag matches any element.
func byClass(tag, class string) func(*html.Node) bool {
	return func(n *html.Node) bool {
		return (tag == "" || n.Data == tag) && hasClass(n, class)
	}
}

// absolute turns a protocol-relative or root-relative href into a full URL
// against the page it was found on. DuckDuckGo's result links are
// protocol-relative, and a "//duckduckgo.com/l/?..." handed back as a URL is
// not one.
func absolute(base, href string) string {
	switch {
	case href == "":
		return ""
	case strings.HasPrefix(href, "//"):
		return "https:" + href
	case strings.HasPrefix(href, "/"):
		return strings.TrimSuffix(base, "/") + href
	default:
		return href
	}
}
