package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode"
)

// ToolHandler runs one tool call. Returning an error is reserved for protocol
// failures; problems the model could correct belong in the result with
// IsError set, which is what toolError produces.
type ToolHandler func(ctx context.Context, args json.RawMessage) (*CallToolResult, *RPCError)

type registeredTool struct {
	def     Tool
	handler ToolHandler
}

// Server dispatches MCP requests. It holds no per-connection state: revision
// 2026-07-28 removed sessions, so every request is answered on its own.
type Server struct {
	name         string
	version      string
	instructions string

	// legacy is whether the initialize-based revisions are served alongside the
	// current one. It decides what server/discover advertises and which
	// versions a request may declare.
	legacy bool

	mu    sync.RWMutex
	tools map[string]registeredTool
	order []string

	// browser is the one browser the scrapers drive, scraper is what the
	// operations are handed, and cfg is what both were configured with. All
	// three are set by main before any request arrives.
	browser *Browser
	scraper *Scraper

	// downloads is the store behind download_asset and the files served from
	// the downloads path.
	downloads *DownloadStore

	cfg Config
}

// NewServer builds a server with no tools registered. legacy enables the
// initialize-based revisions alongside the current one.
func NewServer(name, version, instructions string, legacy bool) *Server {
	return &Server{
		name:         name,
		version:      version,
		instructions: instructions,
		legacy:       legacy,
		tools:        make(map[string]registeredTool),
	}
}

// RegisterTool adds a tool. Later registrations of the same name win, which
// lets a user-defined command shadow a built-in.
func (s *Server) RegisterTool(def Tool, handler ToolHandler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.tools[def.Name]; !exists {
		s.order = append(s.order, def.Name)
	}
	s.tools[def.Name] = registeredTool{def: def, handler: handler}
}

// ToolNames returns the registered tool names in registration order.
func (s *Server) ToolNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]string(nil), s.order...)
}

// versionKey carries the protocol version a transport negotiated for the
// request being handled. A legacy session fixes it once at initialize; a modern
// request declares it in _meta every time.
type versionKey struct{}

// withVersion returns a context carrying the protocol version to answer in.
func withVersion(ctx context.Context, version string) context.Context {
	return context.WithValue(ctx, versionKey{}, version)
}

// versionFrom returns the protocol version for the request being handled.
func versionFrom(ctx context.Context) string {
	version, _ := ctx.Value(versionKey{}).(string)
	return version
}

// completeResult builds the result envelope with this server's identity
// attached, as servers SHOULD include on every result. Against a legacy client
// the envelope is empty: resultType and the cache hints were introduced in
// 2026-07-28 and mean nothing to an earlier revision.
func (s *Server) completeResult(ctx context.Context) Result {
	if !isModernVersion(versionFrom(ctx)) {
		return Result{}
	}
	return Result{
		ResultType: ResultComplete,
		Meta: map[string]any{
			MetaServerInfo: Implementation{Name: s.name, Version: s.version},
		},
	}
}

func (s *Server) capabilities() ServerCapabilities {
	return ServerCapabilities{
		Tools:     &ToolsCapability{ListChanged: false},
		Resources: &ResourcesCapability{},
		Prompts:   &PromptsCapability{},
	}
}

// Handle dispatches a single request and returns its result, or an error. It
// returns (nil, nil) for a notification that needs no response.
//
// When the transport has already negotiated a protocol version - which is what
// a legacy initialize handshake does - it puts it on the context with
// withVersion. Otherwise the version is read from the request metadata, where a
// modern client puts it on every request.
func (s *Server) Handle(ctx context.Context, req *Request) (any, *RPCError) {
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		return nil, Errorf(CodeInvalidRequest, "unsupported jsonrpc version %q", req.JSONRPC)
	}
	if versionFrom(ctx) == "" {
		ctx = withVersion(ctx, req.MetaString(MetaProtocolVersion))
	}
	version := versionFrom(ctx)

	// server/discover is the one method a client may call to find out which
	// versions this server accepts, so it tolerates an absent declaration.
	if req.Method != "server/discover" && !s.supports(version) {
		return nil, UnsupportedVersionError(version)
	}

	switch req.Method {
	case "server/discover":
		return s.discover(ctx), nil
	case "tools/list":
		return s.listTools(ctx), nil
	case "tools/call":
		return s.callTool(ctx, req)
	case "resources/list":
		return s.listResources(ctx)
	case "resources/templates/list":
		return s.listResourceTemplates(ctx), nil
	case "resources/read":
		return s.readResource(ctx, req)
	case "prompts/list":
		return s.listPrompts(ctx), nil
	case "prompts/get":
		return s.getPrompt(ctx, req)

	// Methods that exist only in the legacy revisions. They are accepted so a
	// legacy client is not tripped up by a method-not-found it cannot recover
	// from, but this server has nothing to report through any of them: it
	// publishes no list-changed notifications and logs to stderr.
	case "ping":
		return emptyResult, nil
	case "logging/setLevel", "resources/subscribe", "resources/unsubscribe":
		if isModernVersion(version) {
			return nil, Errorf(CodeMethodNotFound,
				"%q was removed in protocol version %s", req.Method, ProtocolVersion)
		}
		return emptyResult, nil
	case "initialize":
		// Transports handle initialize themselves, because the handshake is
		// what creates the session or fixes the version for the process.
		return nil, Errorf(CodeInvalidRequest, "initialize is handled by the transport")
	}

	if strings.HasPrefix(req.Method, "notifications/") {
		return nil, nil
	}
	return nil, Errorf(CodeMethodNotFound, "unknown method %q", req.Method)
}

// supportedVersions is what this server actually serves, newest first.
func (s *Server) supportedVersions() []string {
	if !s.legacy {
		return ModernVersions
	}
	return SupportedVersions
}

func (s *Server) supports(version string) bool {
	for _, v := range s.supportedVersions() {
		if v == version {
			return true
		}
	}
	return false
}

func (s *Server) discover(ctx context.Context) *DiscoverResult {
	return &DiscoverResult{
		Result:            s.completeResult(ctx).cacheable(3600000, CacheScopePublic),
		SupportedVersions: s.supportedVersions(),
		Capabilities:      s.capabilities(),
		Instructions:      s.instructions,
	}
}

func (s *Server) listTools(ctx context.Context) *ListToolsResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	// The order must be deterministic so clients can cache the list; sorting
	// by name gives that regardless of registration order.
	names := append([]string(nil), s.order...)
	sort.Strings(names)
	tools := make([]Tool, 0, len(names))
	for _, name := range names {
		tools = append(tools, s.tools[name].def)
	}
	return &ListToolsResult{
		Result: s.completeResult(ctx).cacheable(300000, CacheScopePrivate),
		Tools:  tools,
	}
}

func (s *Server) callTool(ctx context.Context, req *Request) (any, *RPCError) {
	var params CallToolParams
	if err := req.Bind(&params); err != nil {
		return nil, err
	}
	s.mu.RLock()
	tool, ok := s.tools[params.Name]
	s.mu.RUnlock()
	if !ok {
		return nil, Errorf(CodeInvalidParams, "unknown tool: %s", params.Name)
	}
	res, err := tool.handler(ctx, params.Arguments)
	if err != nil {
		return nil, err
	}
	res.Result = s.completeResult(ctx)
	return res, nil
}

// toolResult builds a successful tool result from text.
func toolResult(text string) *CallToolResult {
	return &CallToolResult{Content: textContent(text)}
}

// toolResultJSON builds a tool result carrying both structured content and,
// for clients that ignore it, the same data serialised as text.
func toolResultJSON(value any) *CallToolResult {
	pretty, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return toolError("could not serialise result: %v", err)
	}
	return &CallToolResult{
		Content:           textContent(string(pretty)),
		StructuredContent: value,
	}
}

// toolError reports a failure the model can act on and retry.
func toolError(format string, args ...any) *CallToolResult {
	return &CallToolResult{
		Content: textContent(fmt.Sprintf(format, args...)),
		IsError: true,
	}
}

// camelToSnake rewrites a camelCase or PascalCase key as snake_case. Runs of
// capitals are treated as one word, so "maxHTTPRetries" becomes
// "max_http_retries" rather than "max_h_t_t_p_retries".
func camelToSnake(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 4)
	runes := []rune(s)
	for i, r := range runes {
		if !unicode.IsUpper(r) {
			b.WriteRune(r)
			continue
		}
		prevIsLower := i > 0 && !unicode.IsUpper(runes[i-1]) && runes[i-1] != '_'
		nextIsLower := i+1 < len(runes) && unicode.IsLower(runes[i+1])
		if i > 0 && (prevIsLower || nextIsLower) {
			b.WriteByte('_')
		}
		b.WriteRune(unicode.ToLower(r))
	}
	return b.String()
}

// aliasCamelKeys adds a snake_case sibling for every camelCase key, recursively.
// Models are inconsistent about which convention JSON "should" use, and a tool
// call that fails on the spelling of a key wastes a whole turn to fix something
// that carries no meaning. An explicitly supplied snake_case key always wins.
func aliasCamelKeys(v any) any {
	switch t := v.(type) {
	case map[string]any:
		aliases := map[string]any{}
		for k, val := range t {
			t[k] = aliasCamelKeys(val)
			if snake := camelToSnake(k); snake != k {
				if _, exists := t[snake]; !exists {
					aliases[snake] = t[k]
				}
			}
		}
		// Applied after the walk, because adding to a map mid-range leaves it
		// unspecified whether the new keys are visited.
		for k, val := range aliases {
			t[k] = val
		}
		return t
	case []any:
		for i := range t {
			t[i] = aliasCamelKeys(t[i])
		}
		return t
	}
	return v
}

// hasUpper reports whether the payload contains a capital letter at all, which
// is the cheap check that keeps the alias round-trip off the common path.
func hasUpper(raw []byte) bool {
	for _, b := range raw {
		if b >= 'A' && b <= 'Z' {
			return true
		}
	}
	return false
}

// decodeArgs unmarshals tool arguments, reporting a bad shape as a tool error
// rather than a protocol error so the model can correct itself. camelCase keys
// are accepted as aliases for their snake_case equivalents.
func decodeArgs(raw json.RawMessage, v any) *CallToolResult {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	if hasUpper(raw) {
		if aliased, ok := withCamelAliases(raw); ok {
			raw = aliased
		}
	}
	if err := json.Unmarshal(raw, v); err != nil {
		return toolError("invalid arguments: %v", err)
	}
	return nil
}

// withCamelAliases round-trips the payload to add the snake_case aliases. A
// failure here is not reported: the original bytes are then decoded as they
// stand, and any real problem with them surfaces as the usual decode error.
func withCamelAliases(raw json.RawMessage) (json.RawMessage, bool) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber() // keep integers exact through the round trip
	var generic any
	if err := dec.Decode(&generic); err != nil {
		return nil, false
	}
	out, err := json.Marshal(aliasCamelKeys(generic))
	if err != nil {
		return nil, false
	}
	return out, true
}

// schema is a small helper for building JSON Schema 2020-12 object schemas.
func schema(required []string, props map[string]any) map[string]any {
	s := map[string]any{"type": "object"}
	if len(props) == 0 {
		s["additionalProperties"] = false
		return s
	}
	s["properties"] = props
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func prop(typ, description string) map[string]any {
	return map[string]any{"type": typ, "description": description}
}

func propDefault(typ, description string, def any) map[string]any {
	return map[string]any{"type": typ, "description": description, "default": def}
}
