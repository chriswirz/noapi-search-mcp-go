package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
)

func init() {
	registerOperation(&Operation{
		Name:     "geocode",
		Title:    "Address to coordinates",
		Path:     "/geocode",
		ReadOnly: true,
		Summary:  "Turn an address or place name into latitude and longitude.",
		Description: `Turn an address, postcode or place name into latitude and longitude.

It resolves the address the way typing it into Google Maps does, and returns the
coordinates along with the name and full address Google matched it to. Read that
matched address: geocoding an ambiguous string succeeds just as readily as
geocoding an exact one, and the matched address is how you find out which of the
two happened. Confidence says which - "exact" when Google resolved a single
place, "approximate" when it picked the first of several candidates.

No API key and no billing account: this reads maps.google.com rather than the
Geocoding API. That also means it is rate-limited like any other scraper and is
not the right thing to put behind a high-volume service.

Sample prompts:
  - "What are the coordinates of 1600 Amphitheatre Parkway?"
  - "Get the lat/long for the Sydney Opera House"
  - "Geocode 10 Downing Street, London"`,
		Schema: objectSchema([]string{"address"}, map[string]any{
			"address": stringProp("The address, postcode or place name to resolve."),
			"region": stringProp("Country code to bias the result, e.g. \"gb\", \"de\". " +
				"Worth setting for a street address that exists in several countries."),
		}),
		Run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var args struct {
				Address string `json:"address"`
				Region  string `json:"region"`
			}
			if err := bind(raw, &args); err != nil {
				return nil, err
			}
			if strings.TrimSpace(args.Address) == "" {
				return nil, badInput("address is required")
			}
			return s.scraper.geocode(strings.TrimSpace(args.Address), strings.TrimSpace(args.Region))
		},
	})

	registerOperation(&Operation{
		Name:     "google_maps",
		Title:    "Places search",
		Path:     "/maps/search",
		ReadOnly: true,
		Summary:  "Search Google Maps for places, with coordinates for each.",
		Description: `Search Google Maps for places - restaurants, shops, services - and get
back each one's name, rating, category, address and coordinates.

Unlike geocode, which resolves one address, this answers a search: "coffee near
Union Square" returns a list. Every result carries its own latitude and
longitude, so this is also the tool to use when you need coordinates for several
places at once.

Sample prompts:
  - "Find Italian restaurants near Times Square"
  - "Where are the EV charging stations in Reykjavik?"
  - "Top-rated bike shops in Portland"`,
		Schema: objectSchema([]string{"query"}, map[string]any{
			"query":       stringProp("What to look for, e.g. \"pizza near Central Park\"."),
			"num_results": intProp("How many places to return.", 5, 1, 20),
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
			return s.scraper.places(strings.TrimSpace(args.Query),
				clampResults(args.NumResults, 5, 20))
		},
	})

	registerOperation(&Operation{
		Name:     "google_maps_directions",
		Title:    "Directions",
		Path:     "/maps/directions",
		ReadOnly: true,
		Summary:  "Route between two places, with distance and duration.",
		Description: `Get directions between two places, with the distance, the travel time and
the route summary.

Sample prompts:
  - "How far is it from Berlin to Munich by car?"
  - "Walking directions from the Eiffel Tower to the Louvre"
  - "Transit route from Shibuya to Akihabara"

The duration is Google's estimate for the time the page was loaded, traffic
included where Google models it. It is not a schedule.`,
		Schema: objectSchema([]string{"origin", "destination"}, map[string]any{
			"origin":      stringProp("Where the route starts: an address, a place name or \"lat,long\"."),
			"destination": stringProp("Where it ends."),
			"mode": enumProp("How to travel.",
				[]string{"driving", "walking", "transit", "cycling"}, "driving"),
		}),
		Run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var args struct {
				Origin      string `json:"origin"`
				Destination string `json:"destination"`
				Mode        string `json:"mode"`
			}
			if err := bind(raw, &args); err != nil {
				return nil, err
			}
			if strings.TrimSpace(args.Origin) == "" || strings.TrimSpace(args.Destination) == "" {
				return nil, badInput("origin and destination are both required")
			}
			mode := strings.ToLower(strings.TrimSpace(args.Mode))
			if mode == "" {
				mode = "driving"
			}
			if _, ok := travelModes[mode]; !ok {
				return nil, badInput("mode %q: want driving, walking, transit or cycling", mode)
			}
			return s.scraper.directions(strings.TrimSpace(args.Origin),
				strings.TrimSpace(args.Destination), mode)
		},
	})
}

// travelModes maps this server's mode names onto the ones the Maps URL takes.
// Only cycling differs, and it differs every time somebody writes this by hand.
var travelModes = map[string]string{
	"driving": "driving",
	"walking": "walking",
	"transit": "transit",
	"cycling": "bicycling",
}

// ---------------------------------------------------------------------------
// geocode
// ---------------------------------------------------------------------------

// GeocodeResult is one resolved address.
type GeocodeResult struct {
	Query     string  `json:"query"`
	Latitude  float64 `json:"latitude"`
	Longitude float64 `json:"longitude"`

	// Name and Address are what Google matched the query to. Read them: they
	// are the only way to tell an exact hit from a plausible-looking wrong one.
	Name    string `json:"name,omitempty"`
	Address string `json:"address,omitempty"`

	// Confidence is "exact" when Maps resolved the query to a single place,
	// and "approximate" when it returned a list and this is the first of them.
	Confidence string `json:"confidence"`

	// MapsURL is the place on Google Maps, for a person to check.
	MapsURL string `json:"maps_url,omitempty"`
}

func (r *GeocodeResult) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", r.Query)
	fmt.Fprintf(&b, "  %.7f, %.7f  (%s)\n", r.Latitude, r.Longitude, r.Confidence)
	if r.Name != "" {
		fmt.Fprintf(&b, "  Matched: %s\n", r.Name)
	}
	if r.Address != "" && r.Address != r.Name {
		fmt.Fprintf(&b, "  Address: %s\n", r.Address)
	}
	if r.Confidence == "approximate" {
		b.WriteString("  Google returned several candidates and this is the first; " +
			"check the matched address before relying on it.\n")
	}
	if r.MapsURL != "" {
		fmt.Fprintf(&b, "  %s\n", r.MapsURL)
	}
	return strings.TrimRight(b.String(), "\n")
}

// placeCoords matches the coordinates Maps encodes into a resolved place URL.
// The !3d/!4d pair is the place itself; the @lat,long that also appears in the
// URL is the centre of the viewport, which is close but is not the answer -
// zooming out moves it and does not move the place.
var placeCoords = regexp.MustCompile(`!3d(-?\d+\.\d+)!4d(-?\d+\.\d+)`)

// viewportCoords matches the map centre, which is the fallback when a query
// resolves to an area rather than to a point.
var viewportCoords = regexp.MustCompile(`@(-?\d+\.\d+),(-?\d+\.\d+),`)

// placePath matches the name and address Maps puts in the URL path of a
// resolved place.
var placePath = regexp.MustCompile(`/maps/place/([^/@]+)`)

// geocode resolves an address by loading it in Google Maps and reading the
// coordinates back out of the URL.
//
// The URL is the right place to read them from, rather than the page. Maps
// rewrites its own address bar once it has resolved a query, and what it writes
// there is structured, stable and unambiguous - whereas the panel is a rendered
// card whose class names change monthly and which never shows the coordinates
// as numbers at all.
func (s *Scraper) geocode(address, region string) (*GeocodeResult, error) {
	page, err := s.Browser.Acquire()
	if err != nil {
		return nil, err
	}
	defer page.Release()

	target := "https://www.google.com/maps/search/" + url.PathEscape(address) + "/?hl=en"
	if region != "" {
		target += "&gl=" + url.QueryEscape(strings.ToLower(region))
	}
	if err := page.Open(target); err != nil {
		return nil, err
	}
	// Maps resolves the query and then rewrites its URL, which takes a moment
	// and has no event. Poll for it instead of waiting a flat five seconds:
	// most addresses resolve well inside that, and the ones that do not would
	// not have been ready anyway.
	current := page.awaitResolvedURL()

	result := &GeocodeResult{Query: address}
	if m := placeCoords.FindStringSubmatch(current); m != nil {
		result.Latitude, result.Longitude = parseCoords(m[1], m[2])
		result.Confidence = "exact"
	}

	// A query that stays on /maps/search/ resolved to a list rather than to
	// one place. The first card is the best candidate, and its own link
	// carries its coordinates, which is a better answer than the map centre.
	if result.Confidence == "" {
		if first, err := s.firstPlaceFromList(page); err == nil && first != nil {
			result.Latitude, result.Longitude = first.Latitude, first.Longitude
			result.Name, result.Address = first.Name, first.Address
			result.MapsURL = first.MapsURL
			result.Confidence = "approximate"
		}
	}
	if result.Confidence == "" {
		if m := viewportCoords.FindStringSubmatch(current); m != nil {
			result.Latitude, result.Longitude = parseCoords(m[1], m[2])
			result.Confidence = "approximate"
		}
	}
	if result.Confidence == "" {
		return nil, fmt.Errorf("could not resolve %q to coordinates: "+
			"Google Maps did not settle on a place. A more specific address usually fixes it", address)
	}

	if result.Name == "" {
		result.Name, result.Address = placeNameFromURL(current)
	}
	if result.Name == "" {
		// The heading is the fallback for the name, and only the name: it is
		// the one field the URL sometimes omits.
		if heading, err := page.Locator("h1").First().InnerText(); err == nil {
			result.Name = strings.TrimSpace(heading)
		}
	}
	if result.MapsURL == "" && strings.Contains(current, "/maps/place/") {
		result.MapsURL = current
	}
	if result.MapsURL == "" {
		result.MapsURL = fmt.Sprintf("https://www.google.com/maps/@%.7f,%.7f,17z", result.Latitude, result.Longitude)
	}
	return result, nil
}

// awaitResolvedURL waits for Maps to rewrite its address bar to a resolved
// place, returning as soon as it does and giving up after a few seconds with
// whatever the URL says then.
func (p *Page) awaitResolvedURL() string {
	const attempts = 12
	current := p.URL()
	for range attempts {
		if placeCoords.MatchString(current) {
			return current
		}
		p.WaitForTimeout(500)
		current = p.URL()
	}
	return current
}

// placeNameFromURL reads the name and address out of a resolved place URL,
// which encodes them as one comma-separated path segment: the name first, the
// address after it.
func placeNameFromURL(raw string) (name, address string) {
	m := placePath.FindStringSubmatch(raw)
	if m == nil {
		return "", ""
	}
	decoded, err := url.PathUnescape(strings.ReplaceAll(m[1], "+", " "))
	if err != nil {
		return "", ""
	}
	parts := strings.SplitN(decoded, ", ", 2)
	if len(parts) == 2 {
		return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
	}
	return strings.TrimSpace(decoded), ""
}

func parseCoords(lat, long string) (float64, float64) {
	latitude, _ := strconv.ParseFloat(lat, 64)
	longitude, _ := strconv.ParseFloat(long, 64)
	return latitude, longitude
}

// ---------------------------------------------------------------------------
// google_maps
// ---------------------------------------------------------------------------

// Place is one result from a Maps search.
type Place struct {
	Rank      int     `json:"rank"`
	Name      string  `json:"name"`
	Latitude  float64 `json:"latitude,omitempty"`
	Longitude float64 `json:"longitude,omitempty"`
	Rating    string  `json:"rating,omitempty"`
	Reviews   string  `json:"reviews,omitempty"`
	Category  string  `json:"category,omitempty"`
	Address   string  `json:"address,omitempty"`
	Price     string  `json:"price,omitempty"`
	Status    string  `json:"status,omitempty"`
	MapsURL   string  `json:"maps_url,omitempty"`
}

// PlacesResponse is a whole Maps search answer.
type PlacesResponse struct {
	Query   string   `json:"query"`
	Count   int      `json:"count"`
	Places  []Place  `json:"places"`
	Notes   []string `json:"notes,omitempty"`
	MapsURL string   `json:"maps_url,omitempty"`
}

func (r *PlacesResponse) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d places for %q\n", r.Count, r.Query)
	for _, note := range r.Notes {
		fmt.Fprintf(&b, "Note: %s\n", note)
	}
	for _, p := range r.Places {
		fmt.Fprintf(&b, "\n%d. %s\n", p.Rank, p.Name)
		if p.Latitude != 0 || p.Longitude != 0 {
			fmt.Fprintf(&b, "   %.7f, %.7f\n", p.Latitude, p.Longitude)
		}
		if p.Rating != "" {
			fmt.Fprintf(&b, "   Rating: %s\n", joinNonEmpty(" from ", p.Rating, reviewsLabel(p.Reviews)))
		}
		for _, field := range []struct{ label, value string }{
			{"Type", p.Category},
			{"Address", p.Address},
			{"Price", p.Price},
			{"Hours", p.Status},
		} {
			if field.value != "" {
				fmt.Fprintf(&b, "   %s: %s\n", field.label, field.value)
			}
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func reviewsLabel(reviews string) string {
	if reviews == "" {
		return ""
	}
	return reviews + " reviews"
}

// places searches Maps and reads the result cards.
func (s *Scraper) places(query string, want int) (*PlacesResponse, error) {
	page, err := s.Browser.Acquire()
	if err != nil {
		return nil, err
	}
	defer page.Release()

	if err := page.Open("https://www.google.com/maps/search/" + url.PathEscape(query) + "/?hl=en"); err != nil {
		return nil, err
	}
	// The results panel is built after the map, so waiting for a card is the
	// signal - not the canvas, which appears well before there is anything in
	// the list beside it.
	if err := page.WaitForResults("div.Nv2PK"); err != nil {
		if page.Blocked() {
			return nil, ErrBlocked
		}
		// Not fatal: a query that resolves to a single place shows no cards at
		// all, and the extraction handles that case below.
		page.Settle(2000)
	}
	page.Settle(1200)

	var places []Place
	if err := page.EvaluateInto(&places, placesJS, want+3); err != nil {
		return nil, fmt.Errorf("reading the places panel: %w", err)
	}

	resp := &PlacesResponse{Query: query, MapsURL: page.URL()}
	for _, p := range places {
		if strings.TrimSpace(p.Name) == "" {
			continue
		}
		p.Rank = len(resp.Places) + 1
		resp.Places = append(resp.Places, p)
		if len(resp.Places) >= want {
			break
		}
	}
	resp.Count = len(resp.Places)
	if resp.Count == 0 {
		resp.Notes = append(resp.Notes, "no places found; Maps may have resolved this to a single "+
			"location rather than a search, in which case geocode is the tool for it")
	}
	return resp, nil
}

// firstPlaceFromList reads just the first card, which is what geocode falls
// back to when a query does not resolve to one place.
func (s *Scraper) firstPlaceFromList(page *Page) (*Place, error) {
	var places []Place
	if err := page.EvaluateInto(&places, placesJS, 1); err != nil {
		return nil, err
	}
	if len(places) == 0 {
		return nil, nil
	}
	return &places[0], nil
}

// placesJS reads the Maps results panel.
//
// The coordinates come from each card's own link, which encodes them in the
// same !3d/!4d form a resolved place URL uses. That is worth doing rather than
// skipping: coordinates for every result is the difference between a list of
// names and something a caller can actually plot or route between.
const placesJS = `(want) => {
  const out = [];
  const seen = new Set();

  for (const card of document.querySelectorAll('div.Nv2PK')) {
    if (out.length >= want) break;

    const nameEl = card.querySelector('.qBF1Pd, .fontHeadlineSmall, [role="heading"]');
    const link = card.querySelector('a.hfpxzc, a[href*="/maps/place/"]');
    let name = nameEl ? (nameEl.innerText || '').trim() : '';
    if (!name && link) name = (link.getAttribute('aria-label') || '').trim();
    if (!name || name.length < 2 || seen.has(name)) continue;
    seen.add(name);

    let latitude = 0, longitude = 0, mapsUrl = '';
    if (link && link.href) {
      mapsUrl = link.href;
      const m = link.href.match(/!3d(-?[\d.]+)!4d(-?[\d.]+)/);
      if (m) { latitude = parseFloat(m[1]); longitude = parseFloat(m[2]); }
    }

    const ratingEl = card.querySelector('.MW4etd');
    const rating = ratingEl ? (ratingEl.innerText || '').trim() : '';

    // The rest of the card is unlabelled text in a known order, so it is read
    // line by line: a rating line that also carries the review count and the
    // price band, then a "category - address" line, then opening hours.
    let reviews = '', price = '', category = '', address = '', status = '';
    const lines = (card.innerText || '').split('\n').map(s => s.trim()).filter(s => s && s !== ' ');

    for (const line of lines) {
      if (line === name || line.length < 2) continue;

      if (/^\d\.\d/.test(line)) {
        const rev = line.match(/\(([\d,  .]+)\)/);
        if (rev && !reviews) reviews = rev[1].replace(/[  ]/g, ',');
        const band = line.match(/([$€£]{1,4})(?:\s|$)/);
        if (band && !price) price = band[1];
        const range = line.match(/(?:CHF|USD|EUR)?\s*[\d,.]+\s*[-–]\s*[\d,.]+/);
        if (range && !price) price = range[0].trim();
        continue;
      }
      if (/^(Closed|Open\b|Opens|Closes|Temporarily closed|Permanently closed|24 hours|Open 24 hours)/i.test(line)) {
        if (!status) status = line;
        continue;
      }
      if (line.startsWith('"') || line.startsWith('“')) continue;
      if (/^(Reserve|Order online|Dine-in|Takeaway|Takeout|Delivery|Website|Directions|Sponsored)$/i.test(line)) continue;

      if (line.includes('·')) {
        const parts = line.split('·').map(s => s.trim()).filter(s => s.length > 1);
        for (const part of parts) {
          if (/^[$€£]{1,4}$/.test(part)) { if (!price) price = part; }
          else if (!category && !/\d/.test(part) && part.length < 50) category = part;
          else if (!address && /\d/.test(part) && part.length < 90) address = part;
        }
        continue;
      }
    }

    out.push({
      name: name, latitude: latitude, longitude: longitude,
      rating: rating, reviews: reviews, price: price,
      category: category, address: address, status: status,
      maps_url: mapsUrl,
    });
  }
  return out;
}`

// ---------------------------------------------------------------------------
// google_maps_directions
// ---------------------------------------------------------------------------

// Route is one set of directions.
type Route struct {
	Origin      string   `json:"origin"`
	Destination string   `json:"destination"`
	Mode        string   `json:"mode"`
	Distance    string   `json:"distance,omitempty"`
	Duration    string   `json:"duration,omitempty"`
	Summary     string   `json:"summary,omitempty"`
	Steps       []string `json:"steps,omitempty"`
	MapsURL     string   `json:"maps_url,omitempty"`
	Notes       []string `json:"notes,omitempty"`
}

func (r *Route) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s to %s (%s)\n", r.Origin, r.Destination, r.Mode)
	if r.Distance != "" || r.Duration != "" {
		fmt.Fprintf(&b, "  %s\n", joinNonEmpty(", ", r.Distance, r.Duration))
	}
	if r.Summary != "" {
		fmt.Fprintf(&b, "  %s\n", r.Summary)
	}
	for _, note := range r.Notes {
		fmt.Fprintf(&b, "  Note: %s\n", note)
	}
	if len(r.Steps) > 0 {
		b.WriteString("\nSteps:\n")
		for i, step := range r.Steps {
			fmt.Fprintf(&b, "  %d. %s\n", i+1, step)
		}
	}
	if r.MapsURL != "" {
		fmt.Fprintf(&b, "\n%s\n", r.MapsURL)
	}
	return strings.TrimRight(b.String(), "\n")
}

// directions loads a route in Maps and reads the trip panel.
func (s *Scraper) directions(origin, destination, mode string) (*Route, error) {
	page, err := s.Browser.Acquire()
	if err != nil {
		return nil, err
	}
	defer page.Release()

	// Google's documented Maps URL form, rather than the /dir/origin/destination
	// path. The path form ignores travelmode: asking it for driving directions
	// from Berlin to Munich returns a train, silently, and a route in the wrong
	// mode is worse than no route because nothing about it looks wrong.
	params := url.Values{}
	params.Set("api", "1")
	params.Set("origin", origin)
	params.Set("destination", destination)
	params.Set("travelmode", travelModes[mode])
	params.Set("hl", "en")
	target := "https://www.google.com/maps/dir/?" + params.Encode()
	if err := page.Open(target); err != nil {
		return nil, err
	}
	// A route is computed server-side after the page loads, so there is a real
	// element to wait for: the trip card.
	if err := page.WaitForResults("#section-directions-trip-0, [data-trip-index], .MespJc"); err != nil {
		if page.Blocked() {
			return nil, ErrBlocked
		}
		page.Settle(2500)
	}
	page.Settle(1500)

	route := &Route{Origin: origin, Destination: destination, Mode: mode, MapsURL: page.URL()}
	if err := page.EvaluateInto(route, directionsJS); err != nil {
		return nil, fmt.Errorf("reading the directions panel: %w", err)
	}
	// The script fills the measurements and leaves the request fields alone,
	// but a decode into a populated struct can still clear them, so they are
	// restored rather than trusted.
	route.Origin, route.Destination, route.Mode = origin, destination, mode
	route.MapsURL = page.URL()

	if route.Distance == "" && route.Duration == "" {
		route.Notes = append(route.Notes, "Google did not show a distance or a time for this route. "+
			"That usually means one of the two places did not resolve, or there is no route "+
			"in this travel mode - a transit route where there is no transit, most often.")
	}
	return route, nil
}

// directionsJS reads the trip panel: the distance and time from the first
// route, the "via" summary, and the turn list where one is rendered.
const directionsJS = `() => {
  const out = { distance: '', duration: '', summary: '', steps: [] };

  const trip = document.querySelector('#section-directions-trip-0, [data-trip-index="0"], .MespJc');
  const scope = trip || document.body;

  // The trip card is a short list of lines - a duration, a distance, a "via"
  // summary - so it is read line by line rather than by running regular
  // expressions over the whole blob. Matching across a blob is how "4 hr" and
  // the departure time "6:42" get spliced into a duration of "4 hr 6".
  const lines = (scope.innerText || '').split('\n').map(s => s.trim()).filter(Boolean);
  for (const line of lines) {
    if (!out.duration && /^\d+\s*(?:hr|hour|min|day)s?(\s+\d+\s*(?:hr|min)s?)?$/i.test(line)) {
      out.duration = line;
      continue;
    }
    if (!out.distance && /^[\d,.]+\s*(?:km|mi|miles|m|ft)$/i.test(line)) {
      out.distance = line;
      continue;
    }
    if (!out.summary && /^via /i.test(line) && line.length < 80) {
      out.summary = line;
    }
  }

  // Only if the card was not laid out as expected does this fall back to
  // searching the whole panel, where a false match is possible but silence is
  // certain otherwise.
  const text = scope.innerText || '';
  if (!out.distance) {
    const m = text.match(/\b([\d,.]+\s*(?:km|mi|miles))\b/i);
    if (m) out.distance = m[1];
  }
  if (!out.duration) {
    // Hours before minutes: matching minutes first finds the "5 min" inside
    // "1 hr 5 min" and reports a five-minute drive to Munich.
    const m = text.match(/\b(\d+\s*days?(?:\s*\d+\s*hr)?|\d+\s*hr(?:\s*\d+\s*min)?|\d+\s*min)\b/i);
    if (m) out.duration = m[1].trim();
  }

  const seen = new Set();
  for (const step of document.querySelectorAll('.directions-mode-step, [data-legid] .directions-mode-step, .T2yjMc')) {
    const t = (step.innerText || '').trim().replace(/\s*\n\s*/g, ' - ');
    if (t.length > 2 && t.length < 300 && !seen.has(t)) {
      seen.add(t);
      out.steps.push(t);
      if (out.steps.length >= 30) break;
    }
  }
  return out;
}`
