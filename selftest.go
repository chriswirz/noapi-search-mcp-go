package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// The compatibility harness.
//
// Everything this server does rests on assumptions about somebody else's
// website: that Google still marks a result title with an <h3>, that Maps
// still writes coordinates into its own URL, that the patents endpoint still
// answers JSON. Those assumptions are not testable in a unit test, because the
// thing being asserted is the state of a service this process does not control.
// And they break silently: a selector that stops matching produces an empty
// result set, which looks exactly like a query that found nothing.
//
// So the harness runs a known query against each service and checks the answer
// against what that service is supposed to produce. The queries are chosen to
// have obvious, stable answers - the Eiffel Tower has not moved - so that a
// failure means the extraction is wrong rather than the world having changed.
//
// The distinction it exists to draw is between three outcomes that are easy to
// confuse and call for completely different responses:
//
//   - ok: the service answered and the data came back in the expected shape.
//   - blocked: the service refused us - a CAPTCHA, a 429, a bot check. Nothing
//     is wrong with this server. Wait, or use an engine that needs no browser.
//   - broken: the service answered normally and the extraction got nothing, or
//     got something malformed. This is the one that means the code needs
//     fixing, and it is the one that is otherwise invisible.
//
// A harness that reported "blocked" and "broken" the same way would cry wolf
// often enough to be ignored, which is the only way this kind of check fails.

// ProbeStatus is the outcome of one check.
type ProbeStatus string

const (
	// ProbeOK means the service answered and the data was well formed.
	ProbeOK ProbeStatus = "ok"

	// ProbeBlocked means the service refused this address. Environmental, and
	// not a defect in this server.
	ProbeBlocked ProbeStatus = "blocked"

	// ProbeBroken means the service answered but the extraction did not
	// produce what it should have. This is the one worth acting on.
	ProbeBroken ProbeStatus = "broken"

	// ProbeSkipped means the check was not run - it needed a browser that
	// would not start, or it was filtered out.
	ProbeSkipped ProbeStatus = "skipped"
)

// ProbeResult is what one check found.
type ProbeResult struct {
	Name   string      `json:"name"`
	Target string      `json:"target"`
	Status ProbeStatus `json:"status"`

	// Detail says what happened, in the words someone diagnosing it needs. On
	// a broken probe it names the assumption that no longer holds.
	Detail string `json:"detail"`

	// Sample is a fragment of what came back, so a failure can be judged
	// rather than merely believed.
	Sample string `json:"sample,omitempty"`

	NeedsBrowser bool  `json:"needs_browser"`
	DurationMs   int64 `json:"duration_ms"`
}

// SelfTestReport is the whole run.
type SelfTestReport struct {
	StartedAt  string `json:"started_at"`
	DurationMs int64  `json:"duration_ms"`

	OK      int `json:"ok"`
	Blocked int `json:"blocked"`
	Broken  int `json:"broken"`
	Skipped int `json:"skipped"`

	// Healthy is false only when something is broken. A blocked probe does not
	// make the server unhealthy: it is the service declining to talk to this
	// address, which is a fact about the network rather than about the code.
	Healthy bool `json:"healthy"`

	Results []ProbeResult `json:"results"`
	Notes   []string      `json:"notes,omitempty"`
}

// probe is one compatibility check.
type probe struct {
	// name is what the check is called on the command line and in the report.
	name string

	// target names the service, for the report.
	target string

	// needsBrowser marks a probe that cannot run without one, so it is skipped
	// with an explanation rather than failing on a machine that has none.
	needsBrowser bool

	// check runs the probe. Returning an error means the call failed; the
	// harness decides whether that failure was a refusal or a fault. A
	// non-empty string returned alongside a nil error is a broken assumption,
	// stated as the assumption rather than as a stack trace.
	check func(ctx context.Context, s *Scraper) (sample string, broken string, err error)
}

// probesFunc builds the set of checks to run. It is a variable so that the
// harness's own tests can substitute probes with known outcomes: what those
// tests cover is the classification - refusal against fault - and that
// judgement has to be verifiable without depending on what the network is
// doing at the time.
var probesFunc = defaultProbes

// probes is the set of checks for this run.
func probes() []probe { return probesFunc() }

// defaultProbes is every compatibility check, in the order they are reported.
// The browser-free ones come first so that a run on a machine with no Chromium
// still says something useful before it starts skipping.
func defaultProbes() []probe {
	return []probe{
		searchProbe(EngineDuckDuckGo),
		searchProbe(EngineBing),
		searchProbe(EngineMojeek),
		patentsProbe(),
		searchProbe(EngineGoogle),
		searchProbe(EngineBrave),
		searchProbe(EngineStartpage),
		geocodeProbe(),
		placesProbe(),
		directionsProbe(),
		weatherProbe(),
		financeProbe(),
		newsProbe(),
		scholarProbe(),
		fetchProbe(),
	}
}

// searchProbe checks one engine end to end: that it answers, that results come
// back, and that each one carries the fields a caller depends on.
func searchProbe(engineName string) probe {
	engine, _ := LookupEngine(engineName)
	return probe{
		name:         "search:" + engineName,
		target:       engineName,
		needsBrowser: engine != nil && engine.NeedsBrowser(),
		check: func(ctx context.Context, s *Scraper) (string, string, error) {
			// A query with an unambiguous, stable answer. If this returns
			// nothing, the extraction is wrong - not the query.
			q := SearchQuery{Query: "model context protocol specification", NumResults: 5, Page: 1}
			resp, err := engine.Search(ctx, s, q)
			if err != nil {
				return "", "", err
			}
			if len(resp.Results) == 0 {
				return "", "the engine answered but no results were extracted, which means the " +
					"result selectors no longer match this page", nil
			}

			first := resp.Results[0]
			sample := fmt.Sprintf("%s -> %s", first.Title, first.URL)

			if strings.TrimSpace(first.Title) == "" {
				return sample, "results were found but the title is empty", nil
			}
			if !strings.HasPrefix(first.URL, "http") {
				return sample, "the result URL is not absolute: " + first.URL, nil
			}
			// The point of the unwrapping is that a caller gets a destination
			// rather than a link back into the search engine. A tracker
			// surviving here means that mechanism has stopped working, which
			// is invisible from the result count alone.
			if isTracker(first.URL) {
				return sample, "the result URL is still the engine's click tracker, so redirect " +
					"unwrapping has stopped working for this engine", nil
			}
			// Snippets are the other half of what makes a result useful. One
			// missing is normal; all of them missing is a selector.
			withSnippets := 0
			for _, r := range resp.Results {
				if strings.TrimSpace(r.Snippet) != "" {
					withSnippets++
				}
			}
			if withSnippets == 0 {
				return sample, fmt.Sprintf("%d results but not one has a snippet, so the snippet "+
					"selector no longer matches", len(resp.Results)), nil
			}
			return sample, "", nil
		},
	}
}

func patentsProbe() probe {
	return probe{
		name:   "patents",
		target: "patents.google.com",
		// The JSON endpoint needs no browser, though the fallback uses one.
		needsBrowser: false,
		check: func(ctx context.Context, s *Scraper) (string, string, error) {
			q, err := patentArgs{Query: "lithium ion battery", NumResults: 3}.build(s.Search)
			if err != nil {
				return "", "", err
			}
			resp, err := s.patents(ctx, q)
			if err != nil {
				return "", "", err
			}
			if len(resp.Patents) == 0 {
				return "", "the endpoint answered but no patents were extracted; a query this " +
					"broad matching nothing means the response shape has changed", nil
			}
			first := resp.Patents[0]
			sample := fmt.Sprintf("%s %s", first.Number, first.Title)
			if first.Number == "" {
				return sample, "a patent came back with no publication number", nil
			}
			if first.Title == "" {
				return sample, "a patent came back with no title", nil
			}
			// The markup cleaning is easy to lose and hard to notice.
			if strings.Contains(first.Title+first.Snippet+first.Assignee, "<b>") {
				return sample, "highlight markup is reaching callers, so the field cleaning has stopped working", nil
			}
			if first.PriorityDate == "" && first.PublicationDate == "" {
				return sample, "no dates came back, so the date fields have been renamed", nil
			}
			return sample, "", nil
		},
	}
}

func geocodeProbe() probe {
	return probe{
		name:         "geocode",
		target:       "maps.google.com",
		needsBrowser: true,
		check: func(ctx context.Context, s *Scraper) (string, string, error) {
			// A landmark with coordinates that are not going to change, so a
			// wrong answer is unambiguous rather than arguable.
			result, err := s.geocode("Eiffel Tower, Paris", "")
			if err != nil {
				return "", "", err
			}
			sample := fmt.Sprintf("%.5f, %.5f (%s)", result.Latitude, result.Longitude, result.Name)
			if result.Latitude == 0 && result.Longitude == 0 {
				return sample, "no coordinates were found in the resolved URL", nil
			}
			// Within roughly a kilometre of 48.8584, 2.2945. This is the check
			// that catches reading the wrong number out of the URL - the map
			// viewport centre instead of the place, say - which a presence
			// check would pass happily.
			if !near(result.Latitude, 48.8584, 0.02) || !near(result.Longitude, 2.2945, 0.02) {
				return sample, "the coordinates are not where the Eiffel Tower is, so the wrong " +
					"value is being read out of the Maps URL", nil
			}
			return sample, "", nil
		},
	}
}

func placesProbe() probe {
	return probe{
		name:         "maps",
		target:       "maps.google.com",
		needsBrowser: true,
		check: func(ctx context.Context, s *Scraper) (string, string, error) {
			resp, err := s.places("coffee in Portland Oregon", 5)
			if err != nil {
				return "", "", err
			}
			if len(resp.Places) == 0 {
				return "", "no places were extracted from the results panel", nil
			}
			first := resp.Places[0]
			sample := fmt.Sprintf("%s (%.4f, %.4f)", first.Name, first.Latitude, first.Longitude)
			if first.Name == "" {
				return sample, "a place came back with no name", nil
			}
			// Coordinates are dug out of each card's own link, which is the
			// most fragile part of that extraction and the most valuable.
			withCoords := 0
			for _, p := range resp.Places {
				if p.Latitude != 0 || p.Longitude != 0 {
					withCoords++
				}
			}
			if withCoords == 0 {
				return sample, fmt.Sprintf("%d places but none has coordinates, so they are no "+
					"longer encoded in the card links", len(resp.Places)), nil
			}
			return sample, "", nil
		},
	}
}

func directionsProbe() probe {
	return probe{
		name:         "directions",
		target:       "maps.google.com",
		needsBrowser: true,
		check: func(ctx context.Context, s *Scraper) (string, string, error) {
			route, err := s.directions("Berlin", "Munich", "driving")
			if err != nil {
				return "", "", err
			}
			sample := fmt.Sprintf("%s / %s %s", route.Distance, route.Duration, route.Summary)
			if route.Distance == "" && route.Duration == "" {
				return sample, "neither a distance nor a duration was found in the trip panel", nil
			}
			// Berlin to Munich is roughly 585 km by road. A number far from
			// that means the wrong element was read - and in this tool's
			// history, that the travel mode was silently ignored and a train
			// was measured instead of a drive.
			if km := parseKilometres(route.Distance); km > 0 && (km < 400 || km > 900) {
				return sample, fmt.Sprintf("the distance came back as %s, which is not a plausible "+
					"road route from Berlin to Munich; the travel mode may be being ignored", route.Distance), nil
			}
			return sample, "", nil
		},
	}
}

func weatherProbe() probe {
	return probe{
		name:         "weather",
		target:       "google.com/search (weather card)",
		needsBrowser: true,
		check: func(ctx context.Context, s *Scraper) (string, string, error) {
			weather, err := s.weather("Reykjavik")
			if err != nil {
				return "", "", err
			}
			sample := fmt.Sprintf("%s C / %s F, unit %s, %s",
				weather.TempC, weather.TempF, weather.Unit, weather.Condition)
			if weather.TempC == "" && weather.TempF == "" && weather.TempShown == "" {
				return sample, "the weather card gave no temperature", nil
			}
			// The unit toggle is what tells the two readings apart. Losing it
			// is how a Fahrenheit reading gets reported as Celsius, which is
			// wrong in a way nothing downstream can detect.
			if weather.Unit == "" {
				return sample, "the displayed unit could not be determined, so the two temperature " +
					"readings cannot be told apart and may be reported in the wrong scale", nil
			}
			if len(weather.Forecast) == 0 {
				return sample, "no forecast days were extracted", nil
			}
			return sample, "", nil
		},
	}
}

func financeProbe() probe {
	return probe{
		name:         "finance",
		target:       "google.com/finance",
		needsBrowser: true,
		check: func(ctx context.Context, s *Scraper) (string, string, error) {
			quote, err := s.finance("AAPL:NASDAQ")
			if err != nil {
				return "", "", err
			}
			sample := fmt.Sprintf("%s %s %s", quote.Name, quote.Price, quote.Currency)
			if quote.Price == "" {
				return sample, "no price was found", nil
			}
			// The price is rendered as a digit odometer, so a plausibility
			// check is the whole defence against returning "9 8 7 6 5..." as
			// a share price.
			if plausiblePrice(quote.Price) == "" {
				return sample, "the price does not look like a price: " + quote.Price, nil
			}
			return sample, "", nil
		},
	}
}

func newsProbe() probe {
	return probe{
		name:         "news",
		target:       "google.com/search (news)",
		needsBrowser: true,
		check: func(ctx context.Context, s *Scraper) (string, string, error) {
			resp, err := s.vertical(ctx, verticalRequest{
				Tool: "google_news", Mode: "nws", Query: "technology", Want: 5, JS: newsResultsJS,
			})
			if err != nil {
				return "", "", err
			}
			if len(resp.Results) == 0 {
				return "", "no news results were extracted", nil
			}
			first := resp.Results[0]
			sample := fmt.Sprintf("%s (%s)", first.Title, first.Source)
			if isTracker(first.URL) {
				return sample, "the article URL is still Google's redirect, so unwrapping is not " +
					"reaching the vertical results", nil
			}
			return sample, "", nil
		},
	}
}

func scholarProbe() probe {
	return probe{
		name:         "scholar",
		target:       "scholar.google.com",
		needsBrowser: true,
		check: func(ctx context.Context, s *Scraper) (string, string, error) {
			resp, err := s.vertical(ctx, verticalRequest{
				Tool: "google_scholar", Mode: "scholar", Query: "attention is all you need",
				Want: 3, JS: scholarResultsJS,
			})
			if err != nil {
				return "", "", err
			}
			if len(resp.Results) == 0 {
				return "", "no scholar results were extracted", nil
			}
			return resp.Results[0].Title, "", nil
		},
	}
}

func fetchProbe() probe {
	return probe{
		name:         "fetch",
		target:       "arbitrary web pages",
		needsBrowser: true,
		check: func(ctx context.Context, s *Scraper) (string, string, error) {
			// A page that is stable, is not going to block a scraper, and has
			// known text on it.
			page, err := s.fetchPage("https://example.com/", 4000, false)
			if err != nil {
				return "", "", err
			}
			sample := firstLine(page.Text)
			if !strings.Contains(strings.ToLower(page.Text), "example domain") {
				return sample, "the expected text was not found, so the readability extraction is " +
					"stripping too much or returning the wrong element", nil
			}
			return sample, "", nil
		},
	}
}

// RunSelfTest runs the compatibility checks and reports what it found.
//
// The probes run concurrently but with a small limit, because they are pointed
// at services that rate-limit: firing fifteen requests at Google at once is a
// good way to make every probe report "blocked" and learn nothing.
func RunSelfTest(ctx context.Context, s *Scraper, only []string) *SelfTestReport {
	started := time.Now()
	report := &SelfTestReport{StartedAt: started.UTC().Format(time.RFC3339)}

	selected := probes()
	if len(only) > 0 {
		selected = filterProbes(selected, only)
		if len(selected) == 0 {
			report.Notes = append(report.Notes,
				"no probe matched "+strings.Join(only, ", ")+"; run without a filter to see them all")
		}
	}

	// A browser that will not start is established once, so the probes that
	// need one are skipped with a single clear reason instead of fifteen
	// identical launch failures.
	browserAvailable := true
	var browserErr error
	if anyNeedsBrowser(selected) {
		if browserErr = s.Browser.Start(); browserErr != nil {
			browserAvailable = false
			report.Notes = append(report.Notes, "the browser did not start, so the checks that need "+
				"one were skipped: "+browserErr.Error())
		}
	}

	results := make([]ProbeResult, len(selected))
	const parallel = 3
	slots := make(chan struct{}, parallel)
	var wg sync.WaitGroup

	for i, p := range selected {
		wg.Add(1)
		go func(at int, p probe) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			results[at] = runProbe(ctx, s, p, browserAvailable, browserErr)
		}(i, p)
	}
	wg.Wait()

	report.Results = results
	for _, r := range results {
		switch r.Status {
		case ProbeOK:
			report.OK++
		case ProbeBlocked:
			report.Blocked++
		case ProbeBroken:
			report.Broken++
		case ProbeSkipped:
			report.Skipped++
		}
	}
	// Blocked does not mean unhealthy. Being refused by Google says something
	// about this address, not about whether the code is correct, and a check
	// that goes red for that would be turned off within a week.
	report.Healthy = report.Broken == 0
	report.DurationMs = time.Since(started).Milliseconds()

	if report.Blocked > 0 {
		report.Notes = append(report.Notes, fmt.Sprintf(
			"%d service(s) refused this address. That is rate limiting rather than a fault: the "+
				"engines that need no browser (%s) are the ones to use meanwhile.",
			report.Blocked, strings.Join(browserFreeEngines(), ", ")))
	}
	if report.Broken > 0 {
		report.Notes = append(report.Notes, fmt.Sprintf(
			"%d check(s) found a service answering normally but returning data this server could "+
				"not read. That is the case that needs a code change, and the detail on each says "+
				"which assumption stopped holding.", report.Broken))
	}
	return report
}

// runProbe runs one check and classifies the outcome.
func runProbe(ctx context.Context, s *Scraper, p probe, browserAvailable bool, browserErr error) ProbeResult {
	result := ProbeResult{Name: p.name, Target: p.target, NeedsBrowser: p.needsBrowser}
	if p.needsBrowser && !browserAvailable {
		result.Status = ProbeSkipped
		result.Detail = "needs a browser, which did not start: " + browserErr.Error()
		return result
	}

	// Each probe is bounded on its own. One service hanging should cost its
	// own check and not the whole run.
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	started := time.Now()
	sample, broken, err := p.check(ctx, s)
	result.DurationMs = time.Since(started).Milliseconds()
	result.Sample = truncate(cleanText(sample), 200)

	switch {
	case err != nil && isRefusal(err):
		result.Status = ProbeBlocked
		result.Detail = err.Error()
	case err != nil:
		// A call that failed outright, for a reason that is not a refusal:
		// a timeout, a selector that never appeared, a decode failure. From
		// the harness's point of view that is the extraction not working.
		result.Status = ProbeBroken
		result.Detail = err.Error()
	case broken != "":
		result.Status = ProbeBroken
		result.Detail = broken
	default:
		result.Status = ProbeOK
		result.Detail = "answered, and the data was in the expected shape"
	}
	return result
}

// isRefusal reports whether a failure was the service declining rather than
// this server getting it wrong. Getting this line right is what the harness is
// for: everything on the wrong side of it is either a false alarm or a real
// defect reported as weather.
func isRefusal(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{
		"bot check", "rate-limited", "captcha",
		"http 429", "http 503", "http 403",
		"bot challenge", "strict about headless",
		// Mojeek's one-time gate. It is a refusal rather than a fault, but
		// unlike the others it does not clear by waiting, which is why its
		// message says what actually lifts it.
		"verification challenge",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func anyNeedsBrowser(selected []probe) bool {
	for _, p := range selected {
		if p.needsBrowser {
			return true
		}
	}
	return false
}

// filterProbes keeps the probes whose name contains any of the given terms, so
// "--selftest google" runs the Google ones and "--selftest search" runs every
// engine.
func filterProbes(all []probe, only []string) []probe {
	var kept []probe
	for _, p := range all {
		for _, term := range only {
			term = strings.ToLower(strings.TrimSpace(term))
			if term != "" && strings.Contains(strings.ToLower(p.name), term) {
				kept = append(kept, p)
				break
			}
		}
	}
	return kept
}

// Render presents the report as the text a person or a model reads.
func (r *SelfTestReport) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Compatibility check: %d ok, %d blocked, %d broken", r.OK, r.Blocked, r.Broken)
	if r.Skipped > 0 {
		fmt.Fprintf(&b, ", %d skipped", r.Skipped)
	}
	fmt.Fprintf(&b, "  (%.1fs)\n", float64(r.DurationMs)/1000)
	if r.Healthy {
		b.WriteString("Every service that answered returned data this server could read.\n")
	}

	for _, result := range r.Results {
		marker := map[ProbeStatus]string{
			ProbeOK: "ok     ", ProbeBlocked: "blocked", ProbeBroken: "BROKEN ", ProbeSkipped: "skipped",
		}[result.Status]
		fmt.Fprintf(&b, "\n  %s %-18s %s\n", marker, result.Name, result.Target)
		fmt.Fprintf(&b, "          %s\n", result.Detail)
		if result.Sample != "" && result.Status != ProbeSkipped {
			fmt.Fprintf(&b, "          sample: %s\n", firstLine(result.Sample))
		}
	}
	for _, note := range r.Notes {
		fmt.Fprintf(&b, "\n%s\n", note)
	}
	return strings.TrimRight(b.String(), "\n")
}

// near reports whether two coordinates are within tolerance degrees.
func near(got, want, tolerance float64) bool {
	d := got - want
	if d < 0 {
		d = -d
	}
	return d <= tolerance
}

// parseKilometres reads a distance like "584 km" or "363 mi" as kilometres,
// returning 0 when it cannot be read. Miles are converted so the plausibility
// check works whichever units Google chose.
func parseKilometres(distance string) float64 {
	fields := strings.Fields(strings.ReplaceAll(distance, ",", ""))
	if len(fields) < 2 {
		return 0
	}
	var value float64
	if _, err := fmt.Sscanf(fields[0], "%f", &value); err != nil {
		return 0
	}
	switch strings.ToLower(fields[1]) {
	case "km":
		return value
	case "mi", "miles":
		return value * 1.609
	}
	return 0
}

func firstLine(s string) string {
	if at := strings.IndexByte(s, '\n'); at >= 0 {
		return strings.TrimSpace(s[:at])
	}
	return strings.TrimSpace(s)
}

// ---------------------------------------------------------------------------
// The harness as an operation
// ---------------------------------------------------------------------------

func init() {
	registerOperation(&Operation{
		Name:     "selftest",
		Title:    "Compatibility check",
		Path:     "/selftest",
		ReadOnly: true,
		Summary:  "Check that the upstream services still return data this server can read.",
		Description: `Run a live compatibility check against the services this server scrapes.

Every tool here rests on assumptions about someone else's website, and those
assumptions break silently: a selector that stops matching returns an empty
result set, which is indistinguishable from a query that found nothing. This
runs a known query against each service - ones with stable, obvious answers -
and checks the shape of what comes back.

Each check reports one of:
  - ok: the service answered and the data was well formed.
  - blocked: the service refused this address. Rate limiting, not a defect.
    Nothing needs fixing; wait, or use an engine that needs no browser.
  - broken: the service answered normally but the extraction produced nothing
    or produced nonsense. This is the one that means the code is out of date,
    and it names the assumption that stopped holding.

Use it when a tool is returning empty results and you need to know whether the
server is broken or merely being throttled - the two look identical from a
single failed call and call for opposite responses.

Sample prompts:
  - "Check whether the search tools still work"
  - "Is Google blocking us or is the scraper broken?"
  - "Run the compatibility check for duckduckgo and bing"

It makes real requests to real services and takes a minute or two, so it is not
something to call speculatively.`,
		Schema: objectSchema(nil, map[string]any{
			"only": stringProp("Comma-separated terms selecting which checks to run, matched " +
				"against their names: \"google\", \"search\" for every engine, \"geocode,weather\". " +
				"Leave it out to run all of them."),
		}),
		Run: func(ctx context.Context, s *Server, raw json.RawMessage) (any, error) {
			var args struct {
				Only string `json:"only"`
			}
			if err := bind(raw, &args); err != nil {
				return nil, err
			}
			return RunSelfTest(ctx, s.scraper, splitList(args.Only)), nil
		},
	})
}

// probeNames is every check's name, for the help text and the error message
// when a filter matches nothing.
func probeNames() []string {
	all := probes()
	names := make([]string, 0, len(all))
	for _, p := range all {
		names = append(names, p.name)
	}
	sort.Strings(names)
	return names
}
