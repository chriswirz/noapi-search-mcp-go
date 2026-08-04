package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// testServer builds a server with the operations registered and no browser.
// Nothing here touches the network: these tests are about the protocol, the
// schemas and the two surfaces agreeing, and a test that needs Google to be
// reachable is a test that fails for reasons that have nothing to do with the
// code it covers.
func testServer(t *testing.T) (*Server, *Config) {
	t.Helper()
	cfg := DefaultConfig()
	cfg.Server.Transport = "http"
	if err := cfg.Normalize(t.TempDir()); err != nil {
		t.Fatalf("normalizing the default config: %v", err)
	}
	srv := NewServer(cfg.Server.Name, cfg.Server.Version, cfg.Server.Instructions, true)
	srv.cfg = cfg
	srv.registerOperations()
	return srv, &srv.cfg
}

func TestOperationsAreRegisteredAsTools(t *testing.T) {
	srv, _ := testServer(t)
	names := srv.ToolNames()
	if len(names) != len(Operations()) {
		t.Fatalf("registered %d tools for %d operations", len(names), len(Operations()))
	}
	// The tools the Python server this one is modelled on published, plus the
	// two this one adds. A rename here breaks every existing client config, so
	// it should have to be deliberate rather than incidental.
	for _, want := range []string{
		"web_search", "google_search", "google_news", "google_scholar",
		"google_images", "google_books", "google_shopping", "visit_page",
		"google_maps", "google_maps_directions", "geocode",
		"google_weather", "google_finance", "google_translate", "google_trends",
		"google_flights", "google_hotels", "google_lens", "list_images",
	} {
		if !contains(names, want) {
			t.Errorf("tool %q is not registered", want)
		}
	}
}

func TestToolSchemasAreWellFormed(t *testing.T) {
	for _, op := range Operations() {
		if op.Description == "" {
			t.Errorf("%s: no description; a model choosing between tools reads this", op.Name)
		}
		if op.Summary == "" {
			t.Errorf("%s: no summary; the OpenAPI operation needs one", op.Name)
		}
		schema := op.Schema
		if schema["type"] != "object" {
			t.Errorf("%s: input schema is not an object", op.Name)
		}
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Errorf("%s: input schema has no properties", op.Name)
			continue
		}
		for name, raw := range props {
			prop, ok := raw.(map[string]any)
			if !ok {
				t.Errorf("%s.%s: property is not an object", op.Name, name)
				continue
			}
			if prop["type"] == nil {
				t.Errorf("%s.%s: property has no type", op.Name, name)
			}
			if prop["description"] == nil {
				t.Errorf("%s.%s: property has no description", op.Name, name)
			}
		}
		// Every required field must actually exist, or a caller that supplies
		// exactly what is required is still rejected.
		for _, required := range asStrings(schema["required"]) {
			if _, ok := props[required]; !ok {
				t.Errorf("%s: %q is required but is not a property", op.Name, required)
			}
		}
	}
}

// TestEveryOperationIsInTheSpec is the check that keeps the two surfaces
// honest. An operation added without a path is served over MCP only, which is
// allowed; one with a path must appear in the document, or the API and its
// documentation have started to disagree.
func TestEveryOperationIsInTheSpec(t *testing.T) {
	_, cfg := testServer(t)
	spec := buildSpec(cfg)
	paths, ok := spec["paths"].(map[string]any)
	if !ok {
		t.Fatal("the spec has no paths")
	}
	for _, op := range Operations() {
		if op.Path == "" {
			continue
		}
		full := cfg.API.BasePath + op.Path
		entry, ok := paths[full].(map[string]any)
		if !ok {
			t.Errorf("%s: %s is not in the spec", op.Name, full)
			continue
		}
		post, ok := entry["post"].(map[string]any)
		if !ok {
			t.Errorf("%s: %s has no POST operation", op.Name, full)
			continue
		}
		if post["operationId"] != op.Name {
			t.Errorf("%s: operationId is %v", op.Name, post["operationId"])
		}
	}
	if spec["openapi"] != openAPIVersion {
		t.Errorf("openapi version is %v, want %s", spec["openapi"], openAPIVersion)
	}
}

func TestSpecIsValidJSON(t *testing.T) {
	_, cfg := testServer(t)
	data, err := specJSON(cfg)
	if err != nil {
		t.Fatalf("rendering the spec: %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("the rendered spec is not valid JSON: %v", err)
	}
}

func TestOperationNamesAreUnique(t *testing.T) {
	seen := map[string]bool{}
	paths := map[string]string{}
	for _, op := range Operations() {
		if seen[op.Name] {
			t.Errorf("duplicate operation name %q", op.Name)
		}
		seen[op.Name] = true
		if op.Path == "" {
			continue
		}
		if other, ok := paths[op.Path]; ok {
			t.Errorf("%s and %s both claim the path %s", other, op.Name, op.Path)
		}
		paths[op.Path] = op.Name
	}
}

// TestRESTIndexAndHealth exercises the two endpoints that answer without
// touching a browser, which are also the two a deployment will poll.
func TestRESTIndexAndHealth(t *testing.T) {
	srv, cfg := testServer(t)
	srv.browser = NewBrowser(cfg.Browser, cfg.Search, testLogger())
	api, err := NewRESTAPI(srv, cfg, testLogger())
	if err != nil {
		t.Fatalf("building the REST facade: %v", err)
	}

	for _, tc := range []struct {
		path string
		want []string
	}{
		{cfg.API.BasePath, []string{`"operations"`, `"engines"`, `"web_search"`}},
		{cfg.API.BasePath + "/health", []string{`"status":"ok"`, `"browser_running":false`}},
		{cfg.API.SpecPath, []string{`"openapi"`, `"paths"`}},
		{cfg.API.DocsPath, []string{"swagger-ui", "SwaggerUIBundle"}},
	} {
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: status %d", tc.path, rec.Code)
			continue
		}
		for _, want := range tc.want {
			if !strings.Contains(rec.Body.String(), want) {
				t.Errorf("GET %s: body does not contain %q", tc.path, want)
			}
		}
	}
}

// TestRESTRejectsBadInput checks that a caller's mistake is reported as theirs.
// The distinction matters operationally: a 400 is worth fixing and retrying,
// and a 502 is worth waiting out, and a server that answers 502 to a missing
// field sends people looking for a fault that is not there.
func TestRESTRejectsBadInput(t *testing.T) {
	srv, cfg := testServer(t)
	srv.browser = NewBrowser(cfg.Browser, cfg.Search, testLogger())
	srv.scraper = NewScraper(srv.browser, cfg.Search)
	api, err := NewRESTAPI(srv, cfg, testLogger())
	if err != nil {
		t.Fatalf("building the REST facade: %v", err)
	}

	for _, tc := range []struct {
		name, path, body string
		status           int
	}{
		{"no query", "/search", `{}`, http.StatusBadRequest},
		{"unknown engine", "/search", `{"query":"x","engine":"altavista"}`, http.StatusBadRequest},
		{"bad time range", "/search", `{"query":"x","time_range":"last tuesday"}`, http.StatusBadRequest},
		{"page too high", "/search", `{"query":"x","page":99}`, http.StatusBadRequest},
		{"no address", "/geocode", `{}`, http.StatusBadRequest},
		{"no url", "/fetch", `{}`, http.StatusBadRequest},
		{"unfetchable scheme", "/fetch", `{"url":"file:///etc/passwd"}`, http.StatusBadRequest},
		{"bad mode", "/maps/directions", `{"origin":"a","destination":"b","mode":"teleport"}`, http.StatusBadRequest},
		{"malformed json", "/search", `{"query":`, http.StatusBadRequest},
		{"unknown operation", "/nonsense", `{}`, http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, cfg.API.BasePath+tc.path, strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			api.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("status %d, want %d: %s", rec.Code, tc.status, rec.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("the error body is not JSON: %v", err)
			}
			if body["error"] == nil {
				t.Errorf("the error body has no error field: %s", rec.Body.String())
			}
		})
	}
}

// TestRESTRequiresTokenWhenConfigured covers the split the auth model rests
// on: the token guards what can act, and not what merely describes.
func TestRESTRequiresTokenWhenConfigured(t *testing.T) {
	srv, cfg := testServer(t)
	cfg.Server.AuthToken = "s3cret"
	srv.browser = NewBrowser(cfg.Browser, cfg.Search, testLogger())
	api, err := NewRESTAPI(srv, cfg, testLogger())
	if err != nil {
		t.Fatalf("building the REST facade: %v", err)
	}

	get := func(path string, header bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if header {
			req.Header.Set("Authorization", "Bearer s3cret")
		}
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, req)
		return rec
	}

	// The documentation is reachable with no credential. It has to be: a
	// browser cannot put an Authorization header on a URL somebody pasted
	// into it, so a protected docs page is one its only client cannot open.
	for _, path := range []string{
		cfg.API.DocsPath,
		cfg.API.DocsPath + "/swagger-ui.css",
		cfg.API.DocsPath + "/swagger-ui-bundle.js",
		// The spec belongs with the page: Swagger UI fetches it on load,
		// before anyone could have authorised.
		cfg.API.SpecPath,
	} {
		if code := get(path, false).Code; code != http.StatusOK {
			t.Errorf("GET %s without a token: status %d, want 200", path, code)
		}
	}

	// Everything that can actually do something still needs the token.
	for _, path := range []string{cfg.API.BasePath, cfg.API.BasePath + "/health"} {
		rec := get(path, false)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s without a token: status %d, want 401", path, rec.Code)
		}
		if rec.Header().Get("WWW-Authenticate") == "" {
			t.Errorf("GET %s: a 401 should say how to authenticate", path)
		}
		if code := get(path, true).Code; code != http.StatusOK {
			t.Errorf("GET %s with the token: status %d, want 200", path, code)
		}
	}

	post := httptest.NewRequest(http.MethodPost, cfg.API.BasePath+"/search",
		strings.NewReader(`{"query":"x"}`))
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, post)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("POST /search without a token: status %d, want 401", rec.Code)
	}

	// A preflight carries no credentials, so it must be answered before the
	// token is checked or every cross-origin call fails before it is sent.
	rec = httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodOptions, cfg.API.BasePath+"/health", nil))
	if rec.Code != http.StatusNoContent {
		t.Errorf("preflight: status %d, want 204", rec.Code)
	}
}

// TestDocsNeverCarryTheToken is the check that makes serving the documentation
// openly safe.
//
// An earlier version put the token into the page - preauthorizeApiKey, and a
// ?token= on the spec URL - which was tolerable only while the page itself
// demanded the token. Serving it openly with that still in place would hand
// the credential to anyone who asked for the docs, turning a usability fix
// into a disclosure.
func TestDocsNeverCarryTheToken(t *testing.T) {
	const token = "super-secret-value"
	srv, cfg := testServer(t)
	cfg.Server.AuthToken = token
	srv.browser = NewBrowser(cfg.Browser, cfg.Search, testLogger())
	api, err := NewRESTAPI(srv, cfg, testLogger())
	if err != nil {
		t.Fatalf("building the REST facade: %v", err)
	}

	for _, path := range []string{cfg.API.DocsPath, cfg.API.SpecPath} {
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if strings.Contains(rec.Body.String(), token) {
			t.Errorf("GET %s serves the auth token to an unauthenticated caller", path)
		}
	}
	// Nor may it be smuggled in through the UI's pre-authorisation hook.
	if strings.Contains(api.docsHTML(), "preauthorizeApiKey") {
		t.Error("the docs page pre-authorises the UI, which embeds the token in a public page")
	}
	// The spec must still declare the scheme, or there is no Authorize button
	// and the reader has no way to supply the token at all.
	if !strings.Contains(string(api.spec), "bearerAuth") {
		t.Error("the spec declares no bearer scheme, so Swagger UI shows no Authorize button")
	}
	if !strings.Contains(api.docsHTML(), "persistAuthorization") {
		t.Error("the token entered through Authorize is not kept across a reload")
	}
}

// TestTokenIsNotAcceptedInTheQueryString guards against the shortcut that was
// removed. A credential in a URL lands in browser history, proxy logs and
// every Referer header the page emits, and with the docs public there is no
// longer anything it buys.
func TestTokenIsNotAcceptedInTheQueryString(t *testing.T) {
	srv, cfg := testServer(t)
	cfg.Server.AuthToken = "s3cret"
	srv.browser = NewBrowser(cfg.Browser, cfg.Search, testLogger())
	api, err := NewRESTAPI(srv, cfg, testLogger())
	if err != nil {
		t.Fatalf("building the REST facade: %v", err)
	}
	rec := httptest.NewRecorder()
	api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cfg.API.BasePath+"/health?token=s3cret", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("a token in the query string was accepted: status %d", rec.Code)
	}
}

// TestProtectDocsLocksThemDown covers the opt-in for anyone who would rather
// not publish their surface.
func TestProtectDocsLocksThemDown(t *testing.T) {
	srv, cfg := testServer(t)
	cfg.Server.AuthToken = "s3cret"
	cfg.API.ProtectDocs = boolPtr(true)
	srv.browser = NewBrowser(cfg.Browser, cfg.Search, testLogger())
	api, err := NewRESTAPI(srv, cfg, testLogger())
	if err != nil {
		t.Fatalf("building the REST facade: %v", err)
	}
	for _, path := range []string{cfg.API.DocsPath, cfg.API.SpecPath} {
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("with protect_docs, GET %s: status %d, want 401", path, rec.Code)
		}
	}
}

// TestNoTokenMeansEverythingIsOpen checks the default local case is unchanged:
// with no token configured, nothing asks for one.
func TestNoTokenMeansEverythingIsOpen(t *testing.T) {
	srv, cfg := testServer(t)
	srv.browser = NewBrowser(cfg.Browser, cfg.Search, testLogger())
	api, err := NewRESTAPI(srv, cfg, testLogger())
	if err != nil {
		t.Fatalf("building the REST facade: %v", err)
	}
	for _, path := range []string{cfg.API.DocsPath, cfg.API.SpecPath, cfg.API.BasePath, cfg.API.BasePath + "/health"} {
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s with no token configured: status %d, want 200", path, rec.Code)
		}
	}
}

// TestMCPModernRequest walks one request through the stateless revision.
func TestMCPModernRequest(t *testing.T) {
	srv, _ := testServer(t)
	ctx := withVersion(context.Background(), ProtocolVersion)

	result, rpcErr := srv.Handle(ctx, &Request{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "tools/list",
	})
	if rpcErr != nil {
		t.Fatalf("tools/list: %v", rpcErr)
	}
	list, ok := result.(*ListToolsResult)
	if !ok {
		t.Fatalf("tools/list returned %T", result)
	}
	if list.ResultType != ResultComplete {
		t.Errorf("resultType is %q, want %q", list.ResultType, ResultComplete)
	}
	if list.CacheScope == "" || list.TTLMs == 0 {
		t.Error("a list result should carry the cache hints this revision requires")
	}
	if len(list.Tools) == 0 {
		t.Fatal("no tools listed")
	}
	// Sorted by name, so a client can cache the list and compare it.
	for i := 1; i < len(list.Tools); i++ {
		if list.Tools[i-1].Name > list.Tools[i].Name {
			t.Fatalf("the tool list is not sorted: %q before %q", list.Tools[i-1].Name, list.Tools[i].Name)
		}
	}
}

// TestMCPLegacyGetsNoModernFields checks the dual-era behaviour: a client on an
// older revision must not be handed fields that revision never defined.
func TestMCPLegacyGetsNoModernFields(t *testing.T) {
	srv, _ := testServer(t)
	ctx := withVersion(context.Background(), "2025-06-18")

	result, rpcErr := srv.Handle(ctx, &Request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"})
	if rpcErr != nil {
		t.Fatalf("tools/list: %v", rpcErr)
	}
	list := result.(*ListToolsResult)
	if list.ResultType != "" || list.TTLMs != 0 || list.CacheScope != "" {
		t.Errorf("a legacy client was sent 2026-07-28 result fields: %+v", list.Result)
	}
}

func TestMCPUnsupportedVersionIsRefused(t *testing.T) {
	srv, _ := testServer(t)
	ctx := withVersion(context.Background(), "1999-01-01")
	_, rpcErr := srv.Handle(ctx, &Request{JSONRPC: "2.0", ID: json.RawMessage(`1`), Method: "tools/list"})
	if rpcErr == nil {
		t.Fatal("an unknown protocol version was accepted")
	}
	if rpcErr.Code != CodeUnsupportedProtocolVersion {
		t.Errorf("error code %d, want %d", rpcErr.Code, CodeUnsupportedProtocolVersion)
	}
}

// TestToolErrorsAreToolErrors checks that a bad argument comes back as a tool
// result the model can read and correct, not as a protocol error that ends the
// turn with nothing useful in it.
func TestToolErrorsAreToolErrors(t *testing.T) {
	srv, cfg := testServer(t)
	srv.browser = NewBrowser(cfg.Browser, cfg.Search, testLogger())
	srv.scraper = NewScraper(srv.browser, cfg.Search)
	ctx := withVersion(context.Background(), ProtocolVersion)

	result, rpcErr := srv.Handle(ctx, &Request{
		JSONRPC: "2.0",
		ID:      json.RawMessage(`1`),
		Method:  "tools/call",
		Params:  json.RawMessage(`{"name":"web_search","arguments":{"engine":"altavista"}}`),
	})
	if rpcErr != nil {
		t.Fatalf("a bad argument was reported as a protocol error: %v", rpcErr)
	}
	call, ok := result.(*CallToolResult)
	if !ok {
		t.Fatalf("tools/call returned %T", result)
	}
	if !call.IsError {
		t.Fatal("a bad argument was reported as success")
	}
	if len(call.Content) == 0 || call.Content[0].Text == "" {
		t.Fatal("the tool error carries no text for the model to read")
	}
}

func TestResourcesAndPrompts(t *testing.T) {
	srv, _ := testServer(t)
	ctx := withVersion(context.Background(), ProtocolVersion)

	listed, rpcErr := srv.listResources(ctx)
	if rpcErr != nil {
		t.Fatalf("resources/list: %v", rpcErr)
	}
	for _, resource := range listed.(*ListResourcesResult).Resources {
		read, rpcErr := srv.readResource(ctx, &Request{
			Params: json.RawMessage(`{"uri":` + quoteJSON(resource.URI) + `}`),
		})
		if rpcErr != nil {
			t.Errorf("reading %s: %v", resource.URI, rpcErr)
			continue
		}
		contents := read.(*ReadResourceResult).Contents
		if len(contents) == 0 || contents[0].Text == "" {
			t.Errorf("%s came back empty", resource.URI)
			continue
		}
		var decoded any
		if err := json.Unmarshal([]byte(contents[0].Text), &decoded); err != nil {
			t.Errorf("%s is not valid JSON: %v", resource.URI, err)
		}
	}

	prompts := srv.listPrompts(ctx)
	if len(prompts.Prompts) == 0 {
		t.Fatal("no prompts listed")
	}
	got, rpcErr := srv.getPrompt(ctx, &Request{
		Params: json.RawMessage(`{"name":"research","arguments":{"question":"how do tides work"}}`),
	})
	if rpcErr != nil {
		t.Fatalf("prompts/get: %v", rpcErr)
	}
	messages := got.(*GetPromptResult).Messages
	if len(messages) == 0 || !strings.Contains(messages[0].Content.Text, "how do tides work") {
		t.Error("the research prompt did not carry the question through")
	}

	// A prompt that needs an argument must say so rather than producing an
	// instruction with a hole in it.
	if _, rpcErr := srv.getPrompt(ctx, &Request{Params: json.RawMessage(`{"name":"research"}`)}); rpcErr == nil {
		t.Error("the research prompt was rendered with no question")
	}
}

func contains(haystack []string, needle string) bool {
	for _, item := range haystack {
		if item == needle {
			return true
		}
	}
	return false
}

func asStrings(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func quoteJSON(s string) string {
	encoded, _ := json.Marshal(s)
	return string(encoded)
}

// TestTunnelRequiresATokenOrAnExplicitOptOut guards the combination that is
// genuinely dangerous: a public URL that drives a browser. It is refused at
// startup rather than warned about, because this server is usually started by
// an editor or in the background, where a warning goes to a log nobody reads.
func TestTunnelRequiresATokenOrAnExplicitOptOut(t *testing.T) {
	base := func() Config {
		cfg := DefaultConfig()
		cfg.Server.Transport = "http"
		cfg.Tunnel.Enabled = true
		cfg.Tunnel.ServerURL = "https://tunnel.example.com"
		cfg.Tunnel.APIKey = "key"
		return cfg
	}

	cfg := base()
	err := cfg.Normalize(t.TempDir())
	if err == nil {
		t.Fatal("a tunnel with no auth token was accepted")
	}
	if !strings.Contains(err.Error(), "auth_token") {
		t.Errorf("the refusal does not name the missing setting: %v", err)
	}

	// A token makes it fine.
	cfg = base()
	cfg.Server.AuthToken = "s3cret"
	if err := cfg.Normalize(t.TempDir()); err != nil {
		t.Errorf("a tunnel with a token was refused: %v", err)
	}

	// So does one from the environment, since that is the recommended way.
	cfg = base()
	cfg.Server.AuthTokenEnv = "TEST_MCP_TOKEN"
	t.Setenv("TEST_MCP_TOKEN", "s3cret")
	if err := cfg.Normalize(t.TempDir()); err != nil {
		t.Errorf("a tunnel with a token from the environment was refused: %v", err)
	}

	// And so does saying explicitly that an open endpoint is intended.
	cfg = base()
	cfg.Tunnel.AllowAnonymous = true
	if err := cfg.Normalize(t.TempDir()); err != nil {
		t.Errorf("an explicitly anonymous tunnel was refused: %v", err)
	}

	// A local listener with no token is not refused: it is reachable only from
	// this machine, and demanding a credential for that would be theatre.
	cfg = DefaultConfig()
	cfg.Server.Transport = "http"
	if err := cfg.Normalize(t.TempDir()); err != nil {
		t.Errorf("a local HTTP server with no token was refused: %v", err)
	}
}

// TestCDPURLValidation covers the attach-mode setting, whose failure mode is a
// connection error that looks like the port being closed.
func TestCDPURLValidation(t *testing.T) {
	for _, tc := range []struct {
		url     string
		wantErr bool
	}{
		{url: "http://127.0.0.1:9222"},
		{url: "http://localhost:9222"},
		{url: "ws://127.0.0.1:9222/devtools/browser/abc"},
		{url: "https://remote.example.com:9222"},
		{url: "127.0.0.1:9222", wantErr: true}, // no scheme
		{url: "ftp://127.0.0.1:9222", wantErr: true},
		{url: "http://", wantErr: true},
	} {
		cfg := DefaultConfig()
		cfg.Browser.CDPURL = tc.url
		err := cfg.Normalize(t.TempDir())
		if tc.wantErr && err == nil {
			t.Errorf("cdp_url %q was accepted", tc.url)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("cdp_url %q was refused: %v", tc.url, err)
		}
	}
}

// TestSpecServerFollowsTheRequest covers a server reached through a reverse
// proxy: the spec has to name the origin the reader's browser used, or the
// only server Swagger UI offers is the loopback address this process listens
// on and "Try it out" aims somewhere unreachable.
func TestSpecServerFollowsTheRequest(t *testing.T) {
	srv, cfg := testServer(t)
	api, err := NewRESTAPI(srv, cfg, testLogger())
	if err != nil {
		t.Fatalf("building the REST facade: %v", err)
	}

	serverURL := func(r *http.Request) string {
		rec := httptest.NewRecorder()
		api.ServeHTTP(rec, r)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d", cfg.API.SpecPath, rec.Code)
		}
		var spec struct {
			Servers []struct {
				URL string `json:"url"`
			} `json:"servers"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &spec); err != nil {
			t.Fatalf("decoding the spec: %v", err)
		}
		if len(spec.Servers) != 1 {
			t.Fatalf("spec names %d servers, want 1", len(spec.Servers))
		}
		return spec.Servers[0].URL
	}

	req := httptest.NewRequest(http.MethodGet, cfg.API.SpecPath, nil)
	req.Host = "search.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "docs.example.com, internal.example")
	req.Header.Set("X-Forwarded-Prefix", "/search/")
	if got, want := serverURL(req), "https://docs.example.com/search"; got != want {
		t.Errorf("proxied spec names %q, want %q", got, want)
	}

	direct := httptest.NewRequest(http.MethodGet, cfg.API.SpecPath, nil)
	direct.Host = "localhost:8780"
	if got, want := serverURL(direct), "http://localhost:8780"; got != want {
		t.Errorf("direct spec names %q, want %q", got, want)
	}

	// A configured public URL is a deliberate answer and stays put.
	cfg.API.PublicURL = "https://fixed.example.com"
	if api, err = NewRESTAPI(srv, cfg, testLogger()); err != nil {
		t.Fatalf("rebuilding the REST facade: %v", err)
	}
	if got, want := serverURL(req), "https://fixed.example.com"; got != want {
		t.Errorf("configured spec names %q, want %q", got, want)
	}
}
