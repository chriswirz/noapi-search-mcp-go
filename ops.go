package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// An Operation is one thing this server can do, defined once and served twice:
// as an MCP tool and as an HTTP resource described by the OpenAPI document.
//
// Defining it once is the whole point. The alternative - a tool handler and a
// REST handler per capability - is two implementations that drift, and the way
// they drift is that one of them quietly stops matching the documentation. Here
// the input schema is both the tool's inputSchema and the request body schema,
// the description is both the tool description and the operation summary, and
// there is exactly one function that does the work.
type Operation struct {
	// Name is the MCP tool name, and the operationId in the spec.
	Name string

	// Title is a short human label.
	Title string

	// Summary is one line, for the spec's operation summary and the REST
	// index. Description is the full text a model reads when choosing a tool,
	// including the sample prompts that make the choice obvious.
	Summary     string
	Description string

	// Path is the REST path under the configured base, e.g. "/search". Empty
	// means the operation is served over MCP only.
	Path string

	// Schema is the JSON Schema of the arguments, shared by both surfaces.
	Schema map[string]any

	// ReadOnly marks an operation that only reads. Everything here does, which
	// is worth stating: a client is entitled to know that this server changes
	// nothing, and a tool annotation is where it looks.
	ReadOnly bool

	// Run does the work. It returns the structured result; the MCP layer wraps
	// it and the REST layer serialises it.
	//
	// An error here is a real failure - the browser would not start, the
	// engine refused. Something the caller could fix by asking differently is
	// an InputError, which both surfaces report as the caller's problem rather
	// than the server's.
	Run func(ctx context.Context, s *Server, args json.RawMessage) (any, error)
}

// InputError is a problem with the request rather than a failure of the
// server: an unknown engine, a malformed URL, a required field left out. It
// becomes an MCP tool error the model can correct and an HTTP 400.
type InputError struct{ msg string }

func (e *InputError) Error() string { return e.msg }

// badInput builds an InputError.
func badInput(format string, args ...any) error {
	return &InputError{msg: fmt.Sprintf(format, args...)}
}

// Rendered is a result that knows how to present itself as text. Both surfaces
// carry the structured JSON, but a model reading a tool result does better
// with prose than with a nest of objects, and the operation itself is the only
// thing that knows which fields matter.
type Rendered interface {
	Render() string
}

// operations is the registry, in registration order.
var operations []*Operation

// registerOperation adds an operation. It panics on a duplicate name, which is
// a programming mistake rather than a runtime condition: two operations under
// one name would mean the tool list and the spec disagree about what that name
// does.
func registerOperation(op *Operation) {
	for _, existing := range operations {
		if existing.Name == op.Name {
			panic("duplicate operation " + op.Name)
		}
	}
	operations = append(operations, op)
}

// Operations returns every operation, sorted by name so the tool list, the
// REST index and the spec all agree on an order.
func Operations() []*Operation {
	out := append([]*Operation(nil), operations...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// registerOperations installs every operation as an MCP tool.
func (s *Server) registerOperations() {
	for _, op := range Operations() {
		s.RegisterTool(op.Tool(), op.handler(s))
	}
}

// Tool is the operation as an MCP tool definition.
func (op *Operation) Tool() Tool {
	return Tool{
		Name:        op.Name,
		Title:       op.Title,
		Description: op.Description,
		InputSchema: op.Schema,
		Annotations: &ToolAnnotations{
			Title:        op.Title,
			ReadOnlyHint: op.ReadOnly,
			// Every operation here reaches the open web, and a client is
			// entitled to know that before it decides whether to auto-approve.
			OpenWorldHint:  true,
			IdempotentHint: op.ReadOnly,
		},
	}
}

// handler adapts Run to the MCP tool signature.
func (op *Operation) handler(s *Server) ToolHandler {
	return func(ctx context.Context, args json.RawMessage) (*CallToolResult, *RPCError) {
		result, err := op.Run(ctx, s, args)
		if err != nil {
			// Both a bad argument and a blocked scrape come back as tool
			// errors rather than protocol errors: the model can act on either
			// one - correct the argument, or try another engine - and a
			// protocol error would just end the turn.
			return toolError("%s", err), nil
		}
		return renderResult(result), nil
	}
}

// renderResult builds a tool result carrying the structured value and, for the
// model to actually read, the operation's own rendering of it.
func renderResult(value any) *CallToolResult {
	if r, ok := value.(Rendered); ok {
		text := r.Render()
		return &CallToolResult{
			Content:           textContent(text),
			StructuredContent: value,
		}
	}
	return toolResultJSON(value)
}

// bind decodes an operation's arguments, reporting a bad shape as an
// InputError so it reaches the caller as their mistake.
func bind(args json.RawMessage, dest any) error {
	if len(args) == 0 || string(args) == "null" {
		return nil
	}
	raw := args
	if hasUpper(raw) {
		// camelCase keys are accepted as aliases for snake_case ones. Models
		// are inconsistent about which convention JSON "should" use, and a
		// call that fails on the spelling of a key wastes a whole turn fixing
		// something that carries no meaning.
		if aliased, ok := withCamelAliases(raw); ok {
			raw = aliased
		}
	}
	if err := json.Unmarshal(raw, dest); err != nil {
		return badInput("invalid arguments: %v", err)
	}
	return nil
}

// isInputError reports whether an error is the caller's fault.
func isInputError(err error) bool {
	var target *InputError
	return errors.As(err, &target)
}

// clampResults holds num_results between one and the configured ceiling. A
// zero means "unset", which is the common case: it takes the default rather
// than being clamped up to one, so a caller who omits the field gets a useful
// number of results instead of exactly one.
func clampResults(want, def, max int) int {
	if want <= 0 {
		want = def
	}
	if want > max {
		return max
	}
	return want
}

// -- schema helpers ----------------------------------------------------------

// objectSchema builds a JSON Schema for an operation's arguments. The property
// order is not expressible in a Go map, so the spec generator sorts by name and
// leans on required-first ordering instead.
func objectSchema(required []string, props map[string]any) map[string]any {
	s := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func stringProp(description string) map[string]any {
	return map[string]any{"type": "string", "description": description}
}

func stringDefault(description, def string) map[string]any {
	return map[string]any{"type": "string", "description": description, "default": def}
}

func enumProp(description string, values []string, def string) map[string]any {
	s := map[string]any{"type": "string", "description": description, "enum": values}
	if def != "" {
		s["default"] = def
	}
	return s
}

func intProp(description string, def, minimum, maximum int) map[string]any {
	return map[string]any{
		"type":        "integer",
		"description": description,
		"default":     def,
		"minimum":     minimum,
		"maximum":     maximum,
	}
}

func boolProp(description string, def bool) map[string]any {
	return map[string]any{"type": "boolean", "description": description, "default": def}
}

// searchProps are the arguments every general search takes. They are built
// fresh on each call rather than shared, because the schemas are handed to the
// spec generator and to the tool list, and a map shared between them is a map
// one of them can mutate.
func searchProps(withEngine bool) map[string]any {
	props := map[string]any{
		"query":       stringProp("What to search for."),
		"num_results": intProp("How many results to return.", 5, 1, 50),
		"page": intProp("Results page. Use 2, 3 and so on to go past the first page "+
			"rather than asking for more results at once.", 1, 1, 10),
		"site": stringProp("Restrict to one domain, e.g. \"reddit.com\" or \"arxiv.org\". " +
			"Equivalent to the site: operator, which every engine here understands."),
		"time_range": enumProp("Only results published within this window. Engines differ in "+
			"what they support; one that cannot express the window says so in notes "+
			"rather than silently ignoring it.", TimeRanges, ""),
		"language":    stringProp("Language code for results, e.g. \"en\", \"de\", \"ja\"."),
		"region":      stringProp("Country code, e.g. \"us\", \"gb\", \"de\"."),
		"safe_search": boolProp("Ask the engine to filter explicit results.", false),
	}
	if withEngine {
		props["engine"] = enumProp(
			"Which search engine answers. duckduckgo, bing and mojeek need no browser "+
				"and are rarely rate-limited; google and startpage return the fullest "+
				"results; brave and mojeek run their own indexes, so they are the ones "+
				"worth asking when you want a second opinion rather than a fallback.",
			EngineNames(), EngineGoogle)
	}
	return props
}

// toSearchQuery reads the shared search arguments.
type searchArgs struct {
	Query      string `json:"query"`
	Engine     string `json:"engine"`
	NumResults int    `json:"num_results"`
	Page       int    `json:"page"`
	Site       string `json:"site"`
	TimeRange  string `json:"time_range"`
	Language   string `json:"language"`
	Region     string `json:"region"`
	SafeSearch bool   `json:"safe_search"`
}

// query validates the arguments and turns them into a SearchQuery.
func (a searchArgs) query(cfg SearchConfig) (SearchQuery, error) {
	if strings.TrimSpace(a.Query) == "" {
		return SearchQuery{}, badInput("query is required")
	}
	if a.TimeRange != "" {
		valid := false
		for _, r := range TimeRanges {
			if r == a.TimeRange {
				valid = true
				break
			}
		}
		if !valid {
			return SearchQuery{}, badInput("time_range %q: want one of %s",
				a.TimeRange, strings.Join(TimeRanges, ", "))
		}
	}
	page := a.Page
	if page <= 0 {
		page = 1
	}
	if page > 10 {
		return SearchQuery{}, badInput("page %d: the engines stop being useful long before this; ask for at most 10", page)
	}
	return SearchQuery{
		Query:      strings.TrimSpace(a.Query),
		NumResults: clampResults(a.NumResults, 5, cfg.MaxResults),
		Page:       page,
		Site:       strings.TrimSpace(a.Site),
		TimeRange:  a.TimeRange,
		Language:   strings.TrimSpace(a.Language),
		Region:     strings.TrimSpace(a.Region),
		SafeSearch: a.SafeSearch || cfg.SafeSearch,
	}, nil
}
