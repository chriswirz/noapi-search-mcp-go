package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// The tools in this file read Google's answer cards - the weather panel, the
// finance quote, the flight and hotel widgets - rather than a result list. They
// are the most fragile things this server does, because a card is a rendered
// widget with generated class names and no stable structure, and Google changes
// them without notice.
//
// So each one has the same shape: read the fields by their identifiers where
// Google gives identifiers, and keep the card's own text as a fallback. A model
// handed the card's text can still answer the question; a model handed an empty
// object cannot, and neither can it tell whether that means "no data" or "this
// scraper broke".

func init() {
	registerOperation(&Operation{
		Name:     "google_weather",
		Title:    "Weather",
		Path:     "/weather",
		ReadOnly: true,
		Summary:  "Current conditions and forecast for a place.",
		Description: `Get the current weather and the coming days' forecast for any place.

Sample prompts:
  - "What is the weather in Reykjavik?"
  - "Will it rain in London tomorrow?"
  - "How hot is it in Dubai right now?"`,
		Schema: objectSchema([]string{"location"}, map[string]any{
			"location": stringProp("The place, e.g. \"Tokyo\", \"London, UK\", \"90210\"."),
		}),
		Run: func(_ context.Context, s *Server, raw json.RawMessage) (any, error) {
			location, err := oneString(raw, "location")
			if err != nil {
				return nil, err
			}
			return s.scraper.weather(location)
		},
	})

	registerOperation(&Operation{
		Name:     "google_finance",
		Title:    "Stock quote",
		Path:     "/finance",
		ReadOnly: true,
		Summary:  "Share price and market data for a ticker.",
		Description: `Look up a share price and the key statistics beside it.

A ticker qualified with its exchange is what resolves reliably - "AAPL:NASDAQ",
"TSLA:NASDAQ", ".INX:INDEXSP". A bare company name usually works and sometimes
resolves to the wrong listing, so check the company name that comes back.

Prices are delayed and are whatever the page showed when it loaded. Do not
trade on them.

Sample prompts:
  - "What is Apple trading at?"
  - "Look up NVDA:NASDAQ"
  - "How is the S&P 500 doing today?"`,
		Schema: objectSchema([]string{"query"}, map[string]any{
			"query": stringProp("A ticker with its exchange, e.g. \"AAPL:NASDAQ\", or a company name."),
		}),
		Run: func(_ context.Context, s *Server, raw json.RawMessage) (any, error) {
			query, err := oneString(raw, "query")
			if err != nil {
				return nil, err
			}
			return s.scraper.finance(query)
		},
	})

	registerOperation(&Operation{
		Name:     "google_translate",
		Title:    "Translate",
		Path:     "/translate",
		ReadOnly: true,
		Summary:  "Translate text between languages.",
		Description: `Translate text with Google Translate.

Sample prompts:
  - "Translate 'where is the station' into Japanese"
  - "What does 'Feierabend' mean?"
  - "Put this paragraph into Spanish"`,
		Schema: objectSchema([]string{"text", "to_language"}, map[string]any{
			"text":          stringProp("The text to translate."),
			"to_language":   stringProp("Target language: a name like \"Japanese\" or a code like \"ja\"."),
			"from_language": stringProp("Source language. Leave it out to have it detected."),
		}),
		Run: func(_ context.Context, s *Server, raw json.RawMessage) (any, error) {
			var args struct {
				Text         string `json:"text"`
				ToLanguage   string `json:"to_language"`
				FromLanguage string `json:"from_language"`
			}
			if err := bind(raw, &args); err != nil {
				return nil, err
			}
			if strings.TrimSpace(args.Text) == "" {
				return nil, badInput("text is required")
			}
			if strings.TrimSpace(args.ToLanguage) == "" {
				return nil, badInput("to_language is required")
			}
			return s.scraper.translate(args.Text, args.ToLanguage, args.FromLanguage)
		},
	})

	registerCard("google_trends", "/trends", "Search trends",
		"What people are searching for around a topic.",
		`Look up a topic on Google Trends: related topics, related queries and the
interest-over-time panel.

Trends renders its widgets in script well after the page loads, and it is the
least reliable page this server reads. Expect the related queries more often
than the chart.

Sample prompts:
  - "What are people searching about electric vehicles?"
  - "Is Rust more popular than Go right now?"`,
		func(s *Scraper, query string) (*CardResult, error) {
			return s.card(cardRequest{
				Tool:    "google_trends",
				Query:   query,
				URL:     "https://trends.google.com/trends/explore?hl=en&q=" + url.QueryEscape(query),
				Wait:    "#trends-wrapper, .trends-wrapper, [role='main']",
				Settle:  5000,
				Scope:   ".trends-wrapper, [role='main'], main",
				Missing: "Trends showed nothing for this topic. It needs enough search volume to plot, and a narrow topic often has none.",
			})
		})

	registerCard("google_flights", "/flights", "Flights",
		"Flight options and prices between two places.",
		`Search for flights and read back what Google's flight card shows: routes,
prices and durations.

The card is a summary, not a booking system: it shows a handful of options for
the dates Google inferred. For anything real, follow the link it returns.

Sample prompts:
  - "Find flights from Chicago to Lisbon"
  - "How much is a flight from Sydney to Auckland in March?"`,
		func(s *Scraper, query string) (*CardResult, error) {
			return s.card(cardRequest{
				Tool:    "google_flights",
				Query:   query,
				URL:     googleAnswerURL("flights " + query),
				Wait:    "div#search, div#rso",
				Settle:  3000,
				Scope:   "[data-attrid*='flight'], .kp-wholepage, div#search",
				Link:    "a[href*='google.com/travel/flights']",
				Missing: "Google showed no flight card for this. Naming both cities and a month usually produces one.",
			})
		})

	registerCard("google_hotels", "/hotels", "Hotels",
		"Hotels and prices for a place.",
		`Search for hotels and read back Google's accommodation card: names, nightly
prices and ratings.

Prices are for whatever dates Google guessed unless the query names some, so
say the dates in the query if they matter.

Sample prompts:
  - "Find hotels in Porto"
  - "Hotels near Shinjuku station for the 3rd to the 6th of May"`,
		func(s *Scraper, query string) (*CardResult, error) {
			return s.card(cardRequest{
				Tool:    "google_hotels",
				Query:   query,
				URL:     googleAnswerURL("hotels " + query),
				Wait:    "div#search, div#rso",
				Settle:  3000,
				Scope:   "[data-attrid*='hotel'], .kp-wholepage, div#search",
				Link:    "a[href*='google.com/travel/hotels'], a[href*='google.com/travel/search']",
				Missing: "Google showed no hotel card for this. A place name on its own usually produces one.",
			})
		})
}

// oneString reads a single required string argument, which is the shape of
// half the operations here.
func oneString(raw json.RawMessage, field string) (string, error) {
	var args map[string]any
	if err := bind(raw, &args); err != nil {
		return "", err
	}
	value, _ := args[field].(string)
	value = strings.TrimSpace(value)
	if value == "" {
		return "", badInput("%s is required", field)
	}
	return value, nil
}

// registerCard installs one of the text-card operations, which all take a
// single query and return whatever the card said.
func registerCard(name, path, title, summary, description string, run func(*Scraper, string) (*CardResult, error)) {
	registerOperation(&Operation{
		Name:        name,
		Title:       title,
		Path:        path,
		ReadOnly:    true,
		Summary:     summary,
		Description: description,
		Schema: objectSchema([]string{"query"}, map[string]any{
			"query": stringProp("What to look up."),
		}),
		Run: func(_ context.Context, s *Server, raw json.RawMessage) (any, error) {
			query, err := oneString(raw, "query")
			if err != nil {
				return nil, err
			}
			return run(s.scraper, query)
		},
	})
}

// ---------------------------------------------------------------------------
// The generic text card
// ---------------------------------------------------------------------------

// cardRequest describes one answer card to read.
type cardRequest struct {
	Tool   string
	Query  string
	URL    string
	Wait   string // selector that means the page has arrived
	Settle int    // milliseconds to let the widget render after that
	Scope  string // the element whose text is the answer
	Link   string // an optional "see all" link worth returning
	// Missing is what to say when the card is not there, which is a real
	// answer - "Google has no flight card for this" - rather than an error.
	Missing string
}

// CardResult is what one of Google's answer cards said.
type CardResult struct {
	Tool  string `json:"tool"`
	Query string `json:"query"`

	// Text is the card as it read. These widgets have no stable structure
	// worth parsing into fields, and their text is genuinely the answer: a
	// flight card reads as a list of flights and prices, which is what the
	// question was.
	Text string `json:"text,omitempty"`

	// MoreURL is Google's own "see all" link, where the card offered one.
	MoreURL string `json:"more_url,omitempty"`

	Notes []string `json:"notes,omitempty"`
}

func (r *CardResult) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: %s\n", r.Tool, r.Query)
	for _, note := range r.Notes {
		fmt.Fprintf(&b, "Note: %s\n", note)
	}
	if r.Text != "" {
		fmt.Fprintf(&b, "\n%s\n", r.Text)
	}
	if r.MoreURL != "" {
		fmt.Fprintf(&b, "\nMore: %s\n", r.MoreURL)
	}
	return strings.TrimRight(b.String(), "\n")
}

// card loads a page and returns the text of its answer widget.
func (s *Scraper) card(req cardRequest) (*CardResult, error) {
	page, err := s.Browser.Acquire()
	if err != nil {
		return nil, err
	}
	defer page.Release()

	if err := page.Open(req.URL); err != nil {
		return nil, err
	}
	if err := page.WaitForResults(req.Wait); err != nil && page.Blocked() {
		return nil, ErrBlocked
	}
	page.Settle(req.Settle)

	var out struct {
		Text string `json:"text"`
		Link string `json:"link"`
	}
	if err := page.EvaluateInto(&out, cardTextJS, req.Scope, req.Link); err != nil {
		if page.Blocked() {
			return nil, ErrBlocked
		}
		return nil, fmt.Errorf("reading the card: %w", err)
	}

	result := &CardResult{Tool: req.Tool, Query: req.Query, MoreURL: out.Link}
	result.Text = truncate(cleanText(out.Text), 4000)
	if result.Text == "" {
		result.Notes = append(result.Notes, req.Missing)
	}
	return result, nil
}

// cardTextJS reads the text of the first element matching any of the scope
// selectors, plus an optional link.
const cardTextJS = `(scope, linkSelector) => {
  const out = { text: '', link: '' };
  for (const selector of scope.split(',').map(s => s.trim())) {
    const el = document.querySelector(selector);
    if (el && (el.innerText || '').trim().length > 40) {
      out.text = el.innerText;
      break;
    }
  }
  if (linkSelector) {
    const link = document.querySelector(linkSelector);
    if (link) out.link = link.href;
  }
  return out;
}`

// ---------------------------------------------------------------------------
// google_weather
// ---------------------------------------------------------------------------

// Weather is what the weather card showed.
type Weather struct {
	Location string `json:"location"`
	AsOf     string `json:"as_of,omitempty"`

	// TempC and TempF are both filled: Google shows one and keeps the other in
	// the page, so both are available and neither has to be converted here.
	TempC string `json:"temperature_c,omitempty"`
	TempF string `json:"temperature_f,omitempty"`

	// Unit is the scale the card was displaying, "C" or "F". TempShown is the
	// reading in that scale for the case where the unit could not be
	// determined at all - then this is the only temperature reported, and it
	// is reported without a scale rather than with a guessed one.
	Unit      string `json:"unit,omitempty"`
	TempShown string `json:"temperature_shown,omitempty"`

	Condition     string     `json:"condition,omitempty"`
	Precipitation string     `json:"precipitation,omitempty"`
	Humidity      string     `json:"humidity,omitempty"`
	Wind          string     `json:"wind,omitempty"`
	Forecast      []Forecast `json:"forecast,omitempty"`
	Notes         []string   `json:"notes,omitempty"`
}

// Forecast is one day of it.
type Forecast struct {
	Day       string `json:"day"`
	High      string `json:"high,omitempty"`
	Low       string `json:"low,omitempty"`
	Condition string `json:"condition,omitempty"`
}

func (w *Weather) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Weather for %s\n", w.Location)
	if w.AsOf != "" {
		fmt.Fprintf(&b, "  As of: %s\n", w.AsOf)
	}
	switch {
	case w.TempC != "" && w.TempF != "":
		fmt.Fprintf(&b, "  Temperature: %s C (%s F)\n", w.TempC, w.TempF)
	case w.TempC != "":
		fmt.Fprintf(&b, "  Temperature: %s C\n", w.TempC)
	case w.TempF != "":
		fmt.Fprintf(&b, "  Temperature: %s F\n", w.TempF)
	case w.TempShown != "":
		fmt.Fprintf(&b, "  Temperature: %s (Google did not say which scale)\n", w.TempShown)
	}
	for _, field := range []struct{ label, value string }{
		{"Condition", w.Condition},
		{"Precipitation", w.Precipitation},
		{"Humidity", w.Humidity},
		{"Wind", w.Wind},
	} {
		if field.value != "" {
			fmt.Fprintf(&b, "  %s: %s\n", field.label, field.value)
		}
	}
	if len(w.Forecast) > 0 {
		b.WriteString("\nForecast:\n")
		for _, day := range w.Forecast {
			fmt.Fprintf(&b, "  %-5s %s\n", day.Day,
				joinNonEmpty("  ", joinNonEmpty(" / ", day.High, day.Low), day.Condition))
		}
	}
	for _, note := range w.Notes {
		fmt.Fprintf(&b, "\nNote: %s\n", note)
	}
	return strings.TrimRight(b.String(), "\n")
}

// weather reads Google's weather panel. This is the one card with stable
// hooks: the panel has carried the same wob_ element ids for years, which is
// why it is parsed into fields rather than returned as text.
func (s *Scraper) weather(location string) (*Weather, error) {
	page, err := s.Browser.Acquire()
	if err != nil {
		return nil, err
	}
	defer page.Release()

	if err := page.Open(googleAnswerURL("weather " + location)); err != nil {
		return nil, err
	}
	if err := page.WaitForResults("#wob_wc, div#search"); err != nil && page.Blocked() {
		return nil, ErrBlocked
	}
	page.Settle(1500)

	weather := &Weather{}
	if err := page.EvaluateInto(weather, weatherJS); err != nil {
		return nil, fmt.Errorf("reading the weather card: %w", err)
	}
	// Google's own location label is sometimes a bare "Weather"; the caller's
	// wording is better than that.
	if weather.Location == "" || strings.EqualFold(weather.Location, "weather") {
		weather.Location = location
	}
	if weather.TempC == "" && weather.TempF == "" && weather.TempShown == "" {
		return nil, fmt.Errorf("no weather card for %q; try naming the country as well", location)
	}
	return weather, nil
}

// weatherJS reads the weather panel by its element ids.
//
// The unit is the subtle part. Google renders both readings and hides one:
// #wob_tm is the value being shown and #wob_ttm the alternate, and which of
// them is Celsius depends on the units the card chose - which in turn depends
// on where Google thinks the request came from. Reading #wob_tm as Celsius
// unconditionally is how "51" degrees in Reykjavik happens.
//
// So the unit is read off the toggle beside the number: the two unit labels
// are both in the DOM, and the one that is *displayed* is the current unit,
// while the hidden one is the link that would switch to it.
const weatherJS = `() => {
  const text = (id) => {
    const el = document.getElementById(id);
    return el ? (el.innerText || '').trim() : '';
  };
  const visible = (el) => !!el && getComputedStyle(el).display !== 'none';

  const out = {
    location: text('wob_loc'),
    as_of: text('wob_dts'),
    condition: text('wob_dc'),
    precipitation: text('wob_pp'),
    humidity: text('wob_hm'),
    wind: text('wob_ws'),
    forecast: [],
  };

  let unit = '';
  for (const el of document.querySelectorAll('#wob_wc .wob_t')) {
    const label = (el.innerText || '').trim();
    if ((label === '°C' || label === '°F') && visible(el) && el.tagName === 'SPAN') {
      unit = label.slice(1);
      break;
    }
  }

  const shown = text('wob_tm');
  const other = text('wob_ttm');
  if (unit === 'C') {
    out.temperature_c = shown;
    out.temperature_f = other;
  } else if (unit === 'F') {
    out.temperature_f = shown;
    out.temperature_c = other;
  } else {
    // The toggle was not found. Reporting the number without claiming a unit
    // beats guessing: a temperature labelled with the wrong scale is worse
    // than one labelled with none.
    out.temperature_shown = shown;
  }
  out.unit = unit;

  // Each forecast day carries the same pair, displayed and hidden, for its
  // high and its low. Taking only the visible ones keeps them in the same
  // unit as the current reading.
  for (const day of document.querySelectorAll('.wob_df')) {
    const name = day.querySelector('.Z1VzSb, .QrNVmd');
    if (!name) continue;
    const temps = [...day.querySelectorAll('.wob_t')].filter(visible).map(el => (el.innerText || '').trim());
    const icon = day.querySelector('img');
    out.forecast.push({
      day: (name.innerText || '').trim(),
      high: temps.length > 0 ? temps[0] : '',
      low: temps.length > 1 ? temps[1] : '',
      condition: icon ? (icon.alt || '') : '',
    });
  }
  return out;
}`

// ---------------------------------------------------------------------------
// google_finance
// ---------------------------------------------------------------------------

// Quote is what the finance page showed.
type Quote struct {
	Query     string            `json:"query"`
	Name      string            `json:"name,omitempty"`
	Price     string            `json:"price,omitempty"`
	Currency  string            `json:"currency,omitempty"`
	Exchange  string            `json:"exchange,omitempty"`
	Change    string            `json:"change,omitempty"`
	ChangePct string            `json:"change_percent,omitempty"`
	Stats     map[string]string `json:"stats,omitempty"`
	About     string            `json:"about,omitempty"`
	Notes     []string          `json:"notes,omitempty"`
}

func (q *Quote) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", joinNonEmpty(" - ", q.Name, q.Query))
	if q.Price != "" {
		fmt.Fprintf(&b, "  Price: %s\n", joinNonEmpty(" ", q.Price, q.Currency))
	}
	if q.Exchange != "" {
		fmt.Fprintf(&b, "  Exchange: %s\n", q.Exchange)
	}
	if change := joinNonEmpty(" ", q.Change, parenthesise(q.ChangePct)); change != "" {
		fmt.Fprintf(&b, "  Change: %s\n", change)
	}
	if len(q.Stats) > 0 {
		b.WriteString("\nKey statistics:\n")
		for _, key := range sortedKeys(q.Stats) {
			fmt.Fprintf(&b, "  %s: %s\n", key, q.Stats[key])
		}
	}
	if q.About != "" {
		fmt.Fprintf(&b, "\n%s\n", q.About)
	}
	for _, note := range q.Notes {
		fmt.Fprintf(&b, "\nNote: %s\n", note)
	}
	return strings.TrimRight(b.String(), "\n")
}

func parenthesise(s string) string {
	if s == "" {
		return ""
	}
	return "(" + s + ")"
}

// finance reads a Google Finance quote page.
func (s *Scraper) finance(query string) (*Quote, error) {
	page, err := s.Browser.Acquire()
	if err != nil {
		return nil, err
	}
	defer page.Release()

	if err := page.Open("https://www.google.com/finance/quote/" + url.PathEscape(query) + "?hl=en"); err != nil {
		return nil, err
	}
	page.Settle(2000)

	quote := &Quote{Query: query}
	if err := page.EvaluateInto(quote, financeJS); err != nil {
		return nil, fmt.Errorf("reading the quote: %w", err)
	}
	quote.Query = query
	quote.Price = plausiblePrice(quote.Price)

	// Google is migrating Finance to a rewritten page, and which of the two
	// this request is served is decided per profile - so both have to work.
	// The new one carries no data attributes and no stable class names at all,
	// so it is read by finding the ticker the caller asked for and reading the
	// block around it.
	if quote.Price == "" {
		beta := &Quote{}
		if err := page.EvaluateInto(beta, financeBetaJS, query); err == nil {
			if price := plausiblePrice(beta.Price); price != "" {
				quote.Name, quote.Price = beta.Name, price
				quote.Change, quote.ChangePct = beta.Change, beta.ChangePct
				quote.Currency, quote.Exchange = beta.Currency, beta.Exchange
			}
		}
	}

	if quote.Price == "" {
		// The quote URL only resolves for an exchange-qualified ticker. A bare
		// name lands on a "we could not find it" page, and a plain search is
		// what actually answers the question.
		if err := page.Open(googleAnswerURL(query + " stock price")); err != nil {
			return nil, err
		}
		page.Settle(1500)
		fallback := &Quote{}
		if err := page.EvaluateInto(fallback, financeFallbackJS); err == nil {
			quote.Name, quote.Change = fallback.Name, fallback.Change
			quote.Price = plausiblePrice(fallback.Price)
			quote.About = fallback.About
		}
		quote.Notes = append(quote.Notes,
			"read from the search result rather than the quote page: "+
				"qualify the ticker with its exchange, e.g. AAPL:NASDAQ, for the full figures")
	}
	if quote.Price == "" {
		return nil, fmt.Errorf("no price found for %q; qualify the ticker with its exchange, e.g. %q", query, "AAPL:NASDAQ")
	}
	return quote, nil
}

// pricePattern is what a share price looks like: an optional currency symbol
// or code, digits with separators, and an optional currency code after.
var pricePattern = regexp.MustCompile(`^[^\d]{0,4}[\d][\d,. ]*\s*[A-Za-z]{0,4}$`)

// plausiblePrice keeps a price only if it looks like one, and returns "" if
// not so the caller falls through to its next source.
//
// This is a backstop rather than a nicety. Google animates the price as an
// odometer - every digit is a column holding 0 through 9, all present in the
// DOM - so a selector that catches the wrong element returns several hundred
// characters of "9 8 7 6 5 4 3 2 1 0" and it looks, to anything downstream,
// exactly like a successfully scraped price. Filtering in the extraction
// script covers each known case; filtering here covers the ones not yet seen.
func plausiblePrice(price string) string {
	price = strings.TrimSpace(price)
	if price == "" || len(price) > 24 || strings.ContainsAny(price, "\n\r") {
		return ""
	}
	if !pricePattern.MatchString(price) {
		return ""
	}
	return price
}

// financeJS reads the quote page. Google puts the price in a data attribute
// there, which is the most reliable hook anywhere in this server: it exists
// for the page's own scripts, not for its layout.
const financeJS = `() => {
  const out = { stats: {} };
  const attribute = (selector, name) => {
    const el = document.querySelector(selector);
    return el ? (el.getAttribute(name) || '') : '';
  };
  const text = (selector) => {
    const el = document.querySelector(selector);
    return el ? (el.innerText || '').trim() : '';
  };

  out.price = attribute('[data-last-price]', 'data-last-price');
  out.currency = attribute('[data-currency-code]', 'data-currency-code');
  out.exchange = attribute('[data-exchange]', 'data-exchange');
  out.name = text('.zzDege');

  // The rendered price is preferred when it is readable, because it carries
  // the currency and the right number of decimal places. But Google animates
  // that number as an odometer: every digit is a column holding 0 through 9,
  // and all of them are in the DOM, so innerText comes back as several hundred
  // characters of "9\n8\n7\n6\n5...". The settled value is in there too, on
  // its own line, so the block is read line by line and the first line that
  // actually looks like a price is taken. The digit columns cannot match it -
  // they are single characters with no decimal part.
  const display = text('.fxKbKc, .kf1m0');
  if (/^[^\n]{1,20}$/.test(display) && /\d/.test(display)) {
    out.price = display;
  } else if (display) {
    const lines = display.split('\n').map(s => s.trim()).filter(Boolean);
    for (let i = 0; i < lines.length; i++) {
      if (/^[\d,]+\.\d{1,4}$/.test(lines[i])) {
        out.price = lines[i];
        // The currency code follows the number on its own line.
        if (i + 1 < lines.length && /^[A-Za-z]{3}$/.test(lines[i + 1])) {
          out.currency = lines[i + 1];
        }
        break;
      }
    }
  }

  const change = document.querySelector('.rPF6Lc');
  if (change) {
    const lines = (change.innerText || '').split('\n').map(s => s.trim()).filter(Boolean);
    if (lines.length > 0) out.change_percent = lines[0];
    if (lines.length > 1) out.change = lines[1];
  }

  for (const row of document.querySelectorAll('.gyFHrc, .eYanAe .P6K39c')) {
    const label = row.querySelector('.mfs7Fc');
    const value = row.querySelector('.QXDnM');
    if (!label || !value) continue;
    // The label carries a tooltip on a second line; only the first is the name.
    const key = (label.innerText || '').trim().split('\n')[0];
    const val = (value.innerText || '').trim().split('\n')[0];
    if (key && val) out.stats[key] = val;
  }

  const about = document.querySelector('.bLLb2d, .Yfwt5');
  if (about) out.about = (about.innerText || '').trim().slice(0, 600);
  return out;
}`

// financeBetaJS reads Google Finance's rewritten quote page.
//
// That page has no data attributes and no semantic class names - every class
// on it is a generated token like "N6SYTe" that changes with each deploy - so
// there is nothing to select on. What there is, is the ticker: the caller
// asked for "AAPL:NASDAQ" and the page prints exactly that. So the extraction
// anchors on the data rather than the markup, finds the element holding the
// ticker, climbs to the block that also holds a price, and reads the lines.
//
// The block is interleaved with Material icon ligatures - "arrow_upward",
// "add" - which render as glyphs but read as words, so they are filtered out
// by shape: an icon name is lowercase with underscores and no spaces.
const financeBetaJS = `(ticker) => {
  const out = {};
  const wanted = ticker.trim().toUpperCase();

  let anchor = null;
  for (const el of document.querySelectorAll('div, span, h1, h2')) {
    if (el.children.length === 0 && (el.innerText || '').trim().toUpperCase() === wanted) {
      anchor = el;
      break;
    }
  }
  if (!anchor) return out;

  let box = anchor;
  for (let i = 0; i < 8 && box.parentElement; i++) {
    box = box.parentElement;
    if (/[\d,]+\.\d{2}/.test(box.innerText || '')) break;
  }

  // A control label rather than data: an icon ligature ("arrow_upward"), or
  // one of the buttons that sit inside this block. A company name always has
  // either a space or a capital in it, which none of these do.
  const isChrome = (s) =>
    /^[a-z][a-z_]*$/.test(s) ||
    /^(home|add to list|following|follow|compare|line|area|candle|bar|overview|analysis|earnings|financials|holdings)$/i.test(s);

  const lines = (box.innerText || '').split('\n')
    .map(s => s.trim())
    .filter(s => s && s !== '|' && !isChrome(s));

  // Past the ticker: the company name, then the price, then the changes.
  const at = lines.findIndex(s => s.toUpperCase() === wanted);
  const rest = at === -1 ? lines : lines.slice(at + 1);
  for (const line of rest) {
    if (!out.price && /^[^\d]{0,3}[\d,]+\.\d{1,4}$/.test(line)) { out.price = line; continue; }
    // The name sits between the ticker and the price. Once the price has been
    // seen, everything after it is chart controls and section tabs, so the
    // name is only taken from before it.
    if (!out.name && !out.price && !/\d/.test(line) && line.length > 1 && line.length < 60) {
      out.name = line;
      continue;
    }
    if (!out.change_percent && /^[+-][\d.]+%/.test(line)) { out.change_percent = line.match(/^[+-][\d.]+%/)[0]; continue; }
    // The absolute change is parenthesised and may be followed by the period
    // it covers - "(+3.25) Today". Anchoring only at the start takes the
    // first one, which is today's; the later one is the after-hours move.
    if (!out.change) {
      const m = line.match(/^\(([+-][\d,.]+)\)/);
      if (m) { out.change = m[1]; continue; }
    }
  }
  if (wanted.includes(':')) out.exchange = wanted.split(':')[1];

  // The currency is stated once in the market-status line under the price.
  const currency = (box.innerText || '').match(/\b(USD|EUR|GBP|JPY|CHF|CAD|AUD|INR|CNY|HKD|SEK|NOK|DKK)\b/);
  if (currency) out.currency = currency[1];
  return out;
}`

// financeFallbackJS reads the price out of a plain search result, which is
// where a bare company name ends up.
const financeFallbackJS = `() => {
  const out = {};
  const price = document.querySelector('[data-attrid*="Price"], .YMlKec');
  if (price) out.price = (price.innerText || '').trim();
  const name = document.querySelector('[data-attrid="title"], .oPhL2e .PZPZlf');
  if (name) out.name = (name.innerText || '').trim();
  const change = document.querySelector('.JwB6zf, [data-attrid*="change"]');
  if (change) out.change = (change.innerText || '').trim();
  const panel = document.querySelector('.kp-wholepage, .knowledge-panel');
  if (panel) out.about = (panel.innerText || '').trim().slice(0, 800);
  return out;
}`

// ---------------------------------------------------------------------------
// google_translate
// ---------------------------------------------------------------------------

// Translation is one translated passage.
type Translation struct {
	Text     string   `json:"text"`
	From     string   `json:"from,omitempty"`
	To       string   `json:"to"`
	Result   string   `json:"result"`
	Notes    []string `json:"notes,omitempty"`
	Detected string   `json:"detected_language,omitempty"`
}

func (t *Translation) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s -> %s\n\n", joinNonEmpty("", t.From, t.Detected, "auto"), t.To)
	fmt.Fprintf(&b, "Original:\n  %s\n\nTranslation:\n  %s\n", t.Text, t.Result)
	for _, note := range t.Notes {
		fmt.Fprintf(&b, "\nNote: %s\n", note)
	}
	return strings.TrimRight(b.String(), "\n")
}

// languageCodes maps the language names people actually type onto the codes
// Translate takes. An unrecognised name is passed through unchanged, because a
// caller who already knows the code should not have to find it in this table.
var languageCodes = map[string]string{
	"english": "en", "spanish": "es", "french": "fr", "german": "de",
	"italian": "it", "portuguese": "pt", "japanese": "ja", "korean": "ko",
	"chinese": "zh-CN", "mandarin": "zh-CN", "arabic": "ar", "russian": "ru",
	"hindi": "hi", "dutch": "nl", "swedish": "sv", "turkish": "tr",
	"polish": "pl", "thai": "th", "vietnamese": "vi", "indonesian": "id",
	"greek": "el", "hebrew": "he", "czech": "cs", "danish": "da",
	"finnish": "fi", "norwegian": "no", "romanian": "ro", "hungarian": "hu",
	"ukrainian": "uk", "welsh": "cy", "irish": "ga", "icelandic": "is",
}

// languageCode resolves a language name or code to the code Translate wants.
func languageCode(name string) string {
	name = strings.TrimSpace(strings.ToLower(name))
	if code, ok := languageCodes[name]; ok {
		return code
	}
	return name
}

// translate reads a translation out of translate.google.com.
func (s *Scraper) translate(text, to, from string) (*Translation, error) {
	target := languageCode(to)
	source := "auto"
	if from != "" {
		source = languageCode(from)
	}

	page, err := s.Browser.Acquire()
	if err != nil {
		return nil, err
	}
	defer page.Release()

	address := fmt.Sprintf("https://translate.google.com/?sl=%s&tl=%s&op=translate&text=%s",
		url.QueryEscape(source), url.QueryEscape(target), url.QueryEscape(text))
	if err := page.Open(address); err != nil {
		return nil, err
	}
	// The translation arrives over the network after the page renders, so this
	// waits for the output element to actually have text in it rather than for
	// the element to exist.
	page.Settle(2500)

	var out struct {
		Result   string `json:"result"`
		Detected string `json:"detected"`
	}
	if err := page.EvaluateInto(&out, translateJS); err != nil {
		return nil, fmt.Errorf("reading the translation: %w", err)
	}
	result := strings.TrimSpace(out.Result)
	if result == "" {
		return nil, fmt.Errorf("Translate returned nothing for this text; a shorter passage usually works")
	}

	translation := &Translation{Text: text, From: from, To: to, Result: result, Detected: out.Detected}
	if result == text {
		translation.Notes = append(translation.Notes,
			"the translation is identical to the input, which usually means it was already in the target language")
	}
	return translation, nil
}

// translateJS reads the output pane. Translate's own markup changes often, so
// this tries the stable jsname hook first and falls back to the last of the
// text panes - the source is the first, the translation is the last.
const translateJS = `() => {
  const out = { result: '', detected: '' };
  const primary = document.querySelector('[jsname="W297wb"], .ryNqvb, .HwtZe');
  if (primary) out.result = (primary.innerText || '').trim();

  if (!out.result) {
    const panes = document.querySelectorAll('.Y2IQFc');
    if (panes.length >= 2) out.result = (panes[panes.length - 1].innerText || '').trim();
  }

  // The detected language is only reported when it actually looks like one.
  // The element it lives in shares a container with the toolbar, and reading
  // it loosely returns "checkhistoryDetect languageauto_awesome" - the icon
  // ligatures of the buttons around it, which is worse than reporting nothing.
  const detected = document.querySelector('[aria-label*="Detected language"], .qSb8Pe');
  if (detected) {
    const label = (detected.innerText || '').replace(/\s*-\s*Detected.*/i, '').trim();
    if (/^[A-Za-z][A-Za-z ()-]{1,24}$/.test(label) && !/detect/i.test(label)) {
      out.detected = label;
    }
  }
  return out;
}`
