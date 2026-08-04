package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strings"
)

// RESTAPI serves the operations as ordinary HTTP resources, with the OpenAPI
// document that describes them and the Swagger UI that reads it.
//
// It exists because MCP is not the only thing that wants these capabilities. A
// script, a service, a person with curl - none of them should have to speak
// JSON-RPC and none of them should have to be handed a second copy of the
// server. The operations are the same objects the MCP tools are built from, so
// there is nothing here that can drift from what the tools do.
type RESTAPI struct {
	server *Server
	cfg    *Config
	logger *log.Logger

	// spec is rendered once at startup: it cannot change while the process
	// runs, and building it per request would produce an identical answer.
	spec []byte

	// routes maps a full path to the operation it serves.
	routes map[string]*Operation

	mux *http.ServeMux
}

// NewRESTAPI builds the facade. It returns an error only if the spec cannot be
// rendered, which would mean an operation schema that is not valid JSON - a
// programming mistake, and one worth refusing to start over rather than
// serving a document that does not describe the server.
func NewRESTAPI(s *Server, cfg *Config, logger *log.Logger) (*RESTAPI, error) {
	spec, err := specJSON(cfg)
	if err != nil {
		return nil, err
	}
	api := &RESTAPI{
		server: s,
		cfg:    cfg,
		logger: logger,
		spec:   spec,
		routes: map[string]*Operation{},
		mux:    http.NewServeMux(),
	}

	base := cfg.API.BasePath
	for _, op := range Operations() {
		if op.Path == "" {
			continue
		}
		api.routes[base+op.Path] = op
	}

	// Registered longest-first is not needed here - net/http's mux already
	// prefers the more specific pattern - but the subtree pattern is, so that
	// an unknown path under the base answers with this server's own 404 and a
	// pointer to the index rather than net/http's bare one.
	api.mux.HandleFunc(base+"/health", api.handleHealth)
	api.mux.HandleFunc(base, api.handleIndex)
	api.mux.HandleFunc(base+"/", api.handleOperation)
	if cfg.DownloadsServed() {
		api.mux.HandleFunc(cfg.API.DownloadsPath+"/", api.handleDownload)
		api.mux.HandleFunc(cfg.API.DownloadsPath, api.handleDownload)
	}
	api.mux.HandleFunc(cfg.API.SpecPath, api.handleSpec)
	api.mux.HandleFunc(cfg.API.DocsPath, api.handleDocs)
	api.mux.HandleFunc(cfg.API.DocsPath+"/", api.handleDocs)
	return api, nil
}

// Patterns is every path this facade answers on, for the transport to mount.
func (a *RESTAPI) Patterns() []string {
	base := a.cfg.API.BasePath
	patterns := []string{
		base,
		base + "/",
		a.cfg.API.SpecPath,
		a.cfg.API.DocsPath,
		a.cfg.API.DocsPath + "/",
	}
	if a.cfg.DownloadsServed() {
		// The subtree pattern only. The downloads path normally sits under the
		// MCP endpoint's own path, and registering the bare form as well would
		// be registering that endpoint's prefix, which is not this handler's
		// to claim.
		patterns = append(patterns, a.cfg.API.DownloadsPath+"/")
	}
	return patterns
}

// ServeHTTP applies the cross-cutting concerns - CORS, the bearer token - and
// then dispatches.
func (a *RESTAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	// Open to any origin, unlike the JSON-RPC endpoint. The origin allowlist
	// there is a DNS-rebinding defence for a local server a browser should not
	// be able to reach; this facade is meant to be called from anywhere, and
	// the bearer token rather than the origin is what protects it.
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
	h.Set("Access-Control-Max-Age", "86400")

	if r.Method == http.MethodOptions {
		// Answered before the token check: a preflight carries no
		// Authorization header, so demanding one would fail every
		// cross-origin call before the real request was ever sent.
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !a.authorised(r) {
		h.Set("WWW-Authenticate", `Bearer realm="api"`)
		writeAPIError(w, http.StatusUnauthorized, "", "authentication required: send Authorization: Bearer <token>")
		return
	}
	a.mux.ServeHTTP(w, r)
}

// authorised checks the bearer token, if one is configured.
//
// The documentation is exempt, and deliberately. What the token protects is
// the ability to drive a browser - to search, to fetch pages, to do those
// things as whoever owns the profile. The description of how to ask is not
// that: the spec is a list of paths and schemas, and Swagger UI is a static
// page compiled into this binary. Neither carries data, and neither does
// anything.
//
// Protecting them bought nothing and cost the thing they are for. A browser
// cannot send an Authorization header to a URL somebody pasted into it, so the
// docs answered 401 to the one client they exist to serve - and the way round
// that was a token in a query string, which is a credential in browser
// history, proxy logs and every Referer header the page emits. Authorising in
// the UI, which is what the Authorize button is for, is both easier and safer.
//
// api.protect_docs restores the old behaviour for anyone who would rather not
// publish their surface, at the cost of that same awkwardness.
func (a *RESTAPI) authorised(r *http.Request) bool {
	token := a.cfg.Server.AuthTokenValue()
	if token == "" {
		return true
	}
	if a.isPublicPath(r.URL.Path) {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(r.Header.Get("Authorization")), "bearer "+token)
}

// isPublicPath reports whether a path is served without the token: the
// documentation page, its embedded assets, and the spec it reads.
//
// The spec has to be in here with the page. Swagger UI fetches it on load,
// before anyone has had the chance to authorise, and with no way to attach a
// credential to that first request - so a protected spec renders as "failed to
// load API definition" and the Authorize button never becomes reachable.
func (a *RESTAPI) isPublicPath(path string) bool {
	if a.cfg.API.DocsProtected() {
		return false
	}
	docs := a.cfg.API.DocsPath
	if path == a.cfg.API.SpecPath || path == docs || strings.HasPrefix(path, docs+"/") {
		return true
	}
	// Stored files, when the operator has said they may be served openly. The
	// paths are unguessable, which is what makes that a defensible option: the
	// point of hosting a file is often to hand its URL to something that
	// cannot set an Authorization header.
	if a.cfg.Downloads.Public && a.cfg.API.DownloadsPath != "" &&
		strings.HasPrefix(path, a.cfg.API.DownloadsPath+"/") {
		return true
	}
	return false
}

// handleOperation runs one operation.
func (a *RESTAPI) handleOperation(w http.ResponseWriter, r *http.Request) {
	op, ok := a.routes[r.URL.Path]
	if !ok {
		writeAPIError(w, http.StatusNotFound, "",
			"no such operation; GET "+a.cfg.API.BasePath+" lists them, and "+a.cfg.API.DocsPath+" documents them")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST, OPTIONS")
		writeAPIError(w, http.StatusMethodNotAllowed, op.Name, "this operation takes POST with a JSON body")
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, op.Name, "could not read the request body: "+err.Error())
		return
	}
	// An empty body is a valid call for an operation whose arguments are all
	// optional, so it becomes an empty object rather than a parse error.
	if len(strings.TrimSpace(string(body))) == 0 {
		body = []byte("{}")
	}
	if !json.Valid(body) {
		writeAPIError(w, http.StatusBadRequest, op.Name, "the request body is not valid JSON")
		return
	}

	result, err := op.Run(r.Context(), a.server, body)
	if err != nil {
		a.writeRunError(w, op, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// writeRunError maps an operation failure onto a status code. The distinction
// that matters to a caller is whether they can fix it: a bad argument is 400
// and worth retrying differently, a blocked scrape is 502 and worth retrying
// later or against another engine.
func (a *RESTAPI) writeRunError(w http.ResponseWriter, op *Operation, err error) {
	switch {
	case isInputError(err):
		writeAPIError(w, http.StatusBadRequest, op.Name, err.Error())
	case errors.Is(err, context.Canceled):
		// The caller hung up; nothing can be written to them anyway, but the
		// status is recorded honestly for anything watching.
		writeAPIError(w, 499, op.Name, "the request was cancelled")
	default:
		a.logger.Printf("%s: %v", op.Name, err)
		writeAPIError(w, http.StatusBadGateway, op.Name, err.Error())
	}
}

// handleIndex lists the operations.
func (a *RESTAPI) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, OPTIONS")
		writeAPIError(w, http.StatusMethodNotAllowed, "", "the index takes GET")
		return
	}
	type entry struct {
		Name        string         `json:"name"`
		Title       string         `json:"title"`
		Summary     string         `json:"summary"`
		Path        string         `json:"path"`
		Method      string         `json:"method"`
		InputSchema map[string]any `json:"input_schema"`
	}
	list := make([]entry, 0, len(a.routes))
	for _, op := range Operations() {
		if op.Path == "" {
			continue
		}
		list = append(list, entry{
			Name:        op.Name,
			Title:       op.Title,
			Summary:     op.Summary,
			Path:        a.cfg.API.BasePath + op.Path,
			Method:      http.MethodPost,
			InputSchema: op.Schema,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"name":       a.cfg.Server.Name,
		"version":    a.cfg.Server.Version,
		"openapi":    a.cfg.API.SpecPath,
		"docs":       a.cfg.API.DocsPath,
		"mcp":        firstPath(a.cfg),
		"engines":    engineSummaries(),
		"operations": list,
	})
}

// engineSummaries describes the search engines, so a caller reading the index
// can choose one without having to read the tool descriptions.
func engineSummaries() []map[string]any {
	out := make([]map[string]any, 0, len(engines))
	for _, e := range AllEngines() {
		out = append(out, map[string]any{
			"name":          e.Name(),
			"description":   e.Description(),
			"needs_browser": e.NeedsBrowser(),
		})
	}
	return out
}

// handleHealth reports whether the server is up. It deliberately does not
// start the browser: a health check that launches a Chrome is a health check
// that cannot be polled.
func (a *RESTAPI) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, OPTIONS")
		writeAPIError(w, http.StatusMethodNotAllowed, "", "the health check takes GET")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":          "ok",
		"version":         a.cfg.Server.Version,
		"protocol":        ProtocolVersion,
		"browser_running": a.server.browser.Running(),
		"operations":      len(a.routes),
	})
}

// handleSpec serves the OpenAPI document.
func (a *RESTAPI) handleSpec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET, OPTIONS")
		writeAPIError(w, http.StatusMethodNotAllowed, "", "the spec takes GET")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-cache")

	// The prebuilt document names the configured public URL, which is right
	// when there is one and wrong the moment the server is reached through
	// anything else: behind a reverse proxy the only server Swagger UI would
	// offer is the loopback address the process happens to listen on, and
	// "Try it out" aims at a host the reader's browser cannot reach. When no
	// public URL is configured, the request itself says where this server was
	// reached, so the spec is rendered per request to say so.
	spec := a.spec
	if a.cfg.API.PublicURL == "" {
		if base := requestBaseURL(r); base != "" {
			if data, err := specJSONFor(a.cfg, base); err == nil {
				spec = data
			} else {
				a.logger.Printf("rendering the OpenAPI document for %s: %v", base, err)
			}
		}
	}
	if _, err := w.Write(spec); err != nil {
		a.logger.Printf("serving the OpenAPI document: %v", err)
	}
}

// requestBaseURL is the origin this request was made to, as the caller's
// browser sees it: the forwarded scheme, host and path prefix a reverse proxy
// reports when it is in front, and the request's own scheme and Host when it
// is not. It returns "" when there is no usable host, so the caller can fall
// back to the configured value.
func requestBaseURL(r *http.Request) string {
	host := firstForwarded(r.Header.Get("X-Forwarded-Host"))
	if host == "" {
		host = r.Host
	}
	if host == "" {
		return ""
	}

	scheme := firstForwarded(r.Header.Get("X-Forwarded-Proto"))
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}

	// A proxy that mounts this server under a path prefix strips the prefix
	// before forwarding, so the paths in the spec are relative to it and the
	// origin alone would be short by exactly that much.
	prefix := strings.TrimSuffix(firstForwarded(r.Header.Get("X-Forwarded-Prefix")), "/")
	if prefix != "" && !strings.HasPrefix(prefix, "/") {
		prefix = "/" + prefix
	}
	return scheme + "://" + host + prefix
}

// firstForwarded takes the first entry of a comma-separated forwarded header:
// each hop appends its own, and the first is the one closest to the client.
func firstForwarded(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

// writeAPIError writes one error in the shape the spec documents.
func writeAPIError(w http.ResponseWriter, status int, operation, message string) {
	body := map[string]any{"error": message}
	if operation != "" {
		body["operation"] = operation
	}
	writeJSON(w, status, body)
}
