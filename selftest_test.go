package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// The harness itself is tested with fake probes rather than live ones. What
// matters here is the classification - whether a failure is reported as the
// service refusing us or as this server being broken - and that judgement has
// to be right whatever the network happens to be doing on the day.

// TestIsRefusal is the check that decides whether an alert is worth raising.
// Getting it wrong in one direction cries wolf until the harness is ignored;
// in the other it stays quiet while a scraper is silently returning nothing.
func TestIsRefusal(t *testing.T) {
	refusals := []error{
		ErrBlocked,
		fmt.Errorf("google: %w", ErrBlocked),
		errors.New("patents.google.com answered HTTP 503"),
		errors.New("mojeek answered HTTP 429"),
		errors.New("brave returned a page with no results on it. This engine is strict about headless browsers"),
		errors.New("Search blocked: CAPTCHA served"),
	}
	for _, err := range refusals {
		if !isRefusal(err) {
			t.Errorf("should be classified as a refusal: %v", err)
		}
	}

	faults := []error{
		nil,
		errors.New("the results never appeared (waiting for div#search)"),
		errors.New("reading the results: page.Evaluate: TypeError"),
		errors.New("google patents returned something other than the expected JSON"),
		errors.New("no readable text at https://example.com/"),
		errors.New("could not resolve \"x\" to coordinates"),
	}
	for _, err := range faults {
		if isRefusal(err) {
			t.Errorf("should not be classified as a refusal: %v", err)
		}
	}
}

// TestSelfTestClassification runs the harness over probes with known outcomes
// and checks each lands in the right bucket - and, crucially, that a blocked
// service does not make the report unhealthy.
func TestSelfTestClassification(t *testing.T) {
	report := runProbeSet(t, []probe{
		{
			name: "fine", target: "example",
			check: func(context.Context, *Scraper) (string, string, error) {
				return "a sample", "", nil
			},
		},
		{
			name: "refused", target: "example",
			check: func(context.Context, *Scraper) (string, string, error) {
				return "", "", ErrBlocked
			},
		},
		{
			name: "stale-selectors", target: "example",
			check: func(context.Context, *Scraper) (string, string, error) {
				return "the page", "the result selectors no longer match", nil
			},
		},
		{
			name: "failed-outright", target: "example",
			check: func(context.Context, *Scraper) (string, string, error) {
				return "", "", errors.New("the results never appeared")
			},
		},
	})

	if report.OK != 1 || report.Blocked != 1 || report.Broken != 2 {
		t.Fatalf("ok=%d blocked=%d broken=%d, want 1/1/2", report.OK, report.Blocked, report.Broken)
	}
	if report.Healthy {
		t.Error("a report with broken probes is healthy")
	}

	byName := map[string]ProbeResult{}
	for _, r := range report.Results {
		byName[r.Name] = r
	}
	if got := byName["fine"].Status; got != ProbeOK {
		t.Errorf("fine: %s", got)
	}
	if got := byName["refused"].Status; got != ProbeBlocked {
		t.Errorf("refused: %s", got)
	}
	if got := byName["stale-selectors"].Status; got != ProbeBroken {
		t.Errorf("stale-selectors: %s", got)
	}
	// A broken probe must say which assumption stopped holding, or the report
	// tells whoever reads it nothing they can act on.
	if !strings.Contains(byName["stale-selectors"].Detail, "no longer match") {
		t.Errorf("the detail does not name the broken assumption: %q", byName["stale-selectors"].Detail)
	}
	if byName["fine"].Sample == "" {
		t.Error("a successful probe carries no sample, so its result cannot be judged")
	}
}

// TestSelfTestBlockedIsNotUnhealthy is the property the exit code depends on:
// being rate-limited says nothing about whether this server's code is correct,
// so a run that is entirely blocked must still be healthy.
func TestSelfTestBlockedIsNotUnhealthy(t *testing.T) {
	report := runProbeSet(t, []probe{
		{
			name: "a", target: "x",
			check: func(context.Context, *Scraper) (string, string, error) { return "", "", ErrBlocked },
		},
		{
			name: "b", target: "x",
			check: func(context.Context, *Scraper) (string, string, error) {
				return "", "", errors.New("answered HTTP 429")
			},
		},
	})
	if !report.Healthy {
		t.Error("a fully rate-limited run is reported as unhealthy, which would make the check " +
			"go red for something no code change can fix")
	}
	if report.Blocked != 2 {
		t.Errorf("blocked=%d, want 2", report.Blocked)
	}
	if len(report.Notes) == 0 || !strings.Contains(strings.Join(report.Notes, " "), "rate limiting") {
		t.Error("a blocked run does not explain itself")
	}
}

// TestSelfTestSkipsBrowserProbes checks that a machine with no browser gets an
// explanation rather than a wall of identical launch failures.
func TestSelfTestSkipsBrowserProbes(t *testing.T) {
	result := runProbe(context.Background(), nil, probe{
		name: "needs-browser", needsBrowser: true,
		check: func(context.Context, *Scraper) (string, string, error) {
			t.Error("a probe needing a browser was run without one")
			return "", "", nil
		},
	}, false, errors.New("chromium is not installed"))

	if result.Status != ProbeSkipped {
		t.Fatalf("status = %s, want skipped", result.Status)
	}
	if !strings.Contains(result.Detail, "chromium is not installed") {
		t.Errorf("the skip does not say why: %q", result.Detail)
	}
}

func TestFilterProbes(t *testing.T) {
	all := probes()
	for _, tc := range []struct {
		filter   []string
		wantSome bool
		contains string
	}{
		{filter: []string{"search"}, wantSome: true, contains: "search:google"},
		{filter: []string{"google"}, wantSome: true, contains: "search:google"},
		{filter: []string{"geocode", "weather"}, wantSome: true, contains: "weather"},
		{filter: []string{"nonsense"}, wantSome: false},
	} {
		kept := filterProbes(all, tc.filter)
		if tc.wantSome && len(kept) == 0 {
			t.Errorf("%v matched nothing", tc.filter)
			continue
		}
		if !tc.wantSome && len(kept) != 0 {
			t.Errorf("%v matched %d probes", tc.filter, len(kept))
			continue
		}
		if tc.contains != "" {
			found := false
			for _, p := range kept {
				if p.name == tc.contains {
					found = true
				}
			}
			if !found {
				t.Errorf("%v did not select %q", tc.filter, tc.contains)
			}
		}
	}
	// "search" must select every engine, since that is what the documented
	// example promises.
	if got, want := len(filterProbes(all, []string{"search"})), len(EngineNames()); got != want {
		t.Errorf("the search filter matched %d probes, want one per engine (%d)", got, want)
	}
}

// TestEveryEngineHasAProbe is the guard that keeps the harness complete. An
// engine added without a probe is one whose breakage nothing would notice.
func TestEveryEngineHasAProbe(t *testing.T) {
	covered := map[string]bool{}
	for _, p := range probes() {
		covered[strings.TrimPrefix(p.name, "search:")] = true
	}
	for _, name := range EngineNames() {
		if !covered[name] {
			t.Errorf("engine %q has no compatibility probe, so nothing would notice it breaking", name)
		}
	}
	// The tools whose extraction is most fragile should be covered too.
	for _, name := range []string{"geocode", "maps", "directions", "weather", "finance", "patents", "fetch"} {
		if !covered[name] {
			t.Errorf("%q has no compatibility probe", name)
		}
	}
}

func TestProbeNamesAreUniqueAndSorted(t *testing.T) {
	names := probeNames()
	seen := map[string]bool{}
	for i, name := range names {
		if seen[name] {
			t.Errorf("duplicate probe name %q", name)
		}
		seen[name] = true
		if i > 0 && names[i-1] > name {
			t.Errorf("probe names are not sorted: %q before %q", names[i-1], name)
		}
	}
}

func TestSelfTestRenderIsReadable(t *testing.T) {
	report := runProbeSet(t, []probe{
		{
			name: "broken-one", target: "example.com",
			check: func(context.Context, *Scraper) (string, string, error) {
				return "what came back", "the snippet selector no longer matches", nil
			},
		},
	})
	rendered := report.Render()
	for _, want := range []string{"BROKEN", "broken-one", "example.com", "snippet selector", "what came back"} {
		if !strings.Contains(rendered, want) {
			t.Errorf("the rendered report does not mention %q:\n%s", want, rendered)
		}
	}
}

func TestParseKilometres(t *testing.T) {
	for in, want := range map[string]float64{
		"584 km":   584,
		"1,234 km": 1234,
		"363 mi":   584.067,
		"":         0,
		"4 hr":     0,
		"nonsense": 0,
	} {
		got := parseKilometres(in)
		if want == 0 && got != 0 {
			t.Errorf("parseKilometres(%q) = %v, want 0", in, got)
			continue
		}
		if want != 0 && (got < want-1 || got > want+1) {
			t.Errorf("parseKilometres(%q) = %v, want about %v", in, got, want)
		}
	}
}

func TestNear(t *testing.T) {
	if !near(48.8584, 48.8590, 0.02) {
		t.Error("coordinates a few hundred metres apart were not considered near")
	}
	if near(48.8584, 51.5074, 0.02) {
		t.Error("Paris and London were considered near")
	}
	// The sign of the difference must not matter.
	if !near(-122.0855, -122.0860, 0.02) {
		t.Error("negative coordinates were mishandled")
	}
}

// TestSelfTestIsRegistered checks the harness reached both surfaces, since
// that is what makes it usable from a model and from a monitor.
func TestSelfTestIsRegistered(t *testing.T) {
	srv, cfg := testServer(t)
	if !contains(srv.ToolNames(), "selftest") {
		t.Error("selftest is not registered as an MCP tool")
	}
	paths := buildSpec(cfg)["paths"].(map[string]any)
	if _, ok := paths[cfg.API.BasePath+"/selftest"]; !ok {
		t.Error("selftest is not in the OpenAPI document")
	}
}

// runProbeSet runs a fixed set of probes through the harness. The probes here
// never touch the network, so no scraper or browser is needed.
func runProbeSet(t *testing.T, set []probe) *SelfTestReport {
	t.Helper()
	original := probesFunc
	defer func() { probesFunc = original }()
	probesFunc = func() []probe { return set }
	return RunSelfTest(context.Background(), nil, nil)
}
