package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// This server publishes a few resources describing itself: the engine list,
// the operation list and the OpenAPI document. They are here because a client
// that wants to know what this server can do should be able to read it rather
// than be told, and because the OpenAPI document is genuinely useful to hand to
// a model that is about to call the HTTP facade.

const (
	engineResourceURI = "noapi://engines"
	toolsResourceURI  = "noapi://operations"
	specResourceURI   = "noapi://openapi.json"
)

func (s *Server) listResources(ctx context.Context) (any, *RPCError) {
	return &ListResourcesResult{
		Result: s.completeResult(ctx).cacheable(3600000, CacheScopePublic),
		Resources: []Resource{
			{
				URI:         engineResourceURI,
				Name:        "engines",
				Title:       "Search engines",
				Description: "Every engine web_search accepts, what each is good for, and whether it needs the browser.",
				MimeType:    "application/json",
			},
			{
				URI:         toolsResourceURI,
				Name:        "operations",
				Title:       "Operations",
				Description: "Every capability this server serves, with its input schema and its HTTP path.",
				MimeType:    "application/json",
			},
			{
				URI:         specResourceURI,
				Name:        "openapi",
				Title:       "OpenAPI document",
				Description: "The OpenAPI 3.1 description of the HTTP facade, the same document served at the spec path.",
				MimeType:    "application/json",
			},
		},
	}, nil
}

func (s *Server) listResourceTemplates(ctx context.Context) *ListResourceTemplatesResult {
	// Nothing here is parameterised: the three resources are fixed documents
	// about this server, not a family of addressable things.
	return &ListResourceTemplatesResult{
		Result:            s.completeResult(ctx).cacheable(3600000, CacheScopePublic),
		ResourceTemplates: []ResourceTemplate{},
	}
}

func (s *Server) readResource(ctx context.Context, req *Request) (any, *RPCError) {
	var params struct {
		URI string `json:"uri"`
	}
	if err := req.Bind(&params); err != nil {
		return nil, err
	}

	var body any
	switch params.URI {
	case engineResourceURI:
		body = map[string]any{"engines": engineSummaries(), "default": s.cfg.Search.Engine}
	case toolsResourceURI:
		body = map[string]any{"operations": operationSummaries(s.cfg.API.BasePath)}
	case specResourceURI:
		body = buildSpec(&s.cfg)
	default:
		return nil, Errorf(CodeInvalidParams, "unknown resource %q", params.URI)
	}

	encoded, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		return nil, Errorf(CodeInternalError, "encoding %s: %v", params.URI, err)
	}
	return &ReadResourceResult{
		Result: s.completeResult(ctx).cacheable(3600000, CacheScopePublic),
		Contents: []ResourceContents{{
			URI:      params.URI,
			MimeType: "application/json",
			Text:     string(encoded),
		}},
	}, nil
}

// operationSummaries describes every operation, for the resource and for
// anything else that wants the list without the whole spec around it.
func operationSummaries(base string) []map[string]any {
	out := make([]map[string]any, 0, len(operations))
	for _, op := range Operations() {
		entry := map[string]any{
			"name":         op.Name,
			"title":        op.Title,
			"summary":      op.Summary,
			"input_schema": op.Schema,
		}
		if op.Path != "" {
			entry["http"] = map[string]any{"method": "POST", "path": base + op.Path}
		}
		out = append(out, entry)
	}
	return out
}

// describeEngines is the engine table as text, for the prompt and the banner.
func describeEngines() string {
	var b strings.Builder
	for _, e := range AllEngines() {
		browser := "no browser"
		if e.NeedsBrowser() {
			browser = "needs the browser"
		}
		fmt.Fprintf(&b, "  %-12s (%s) %s\n", e.Name(), browser, e.Description())
	}
	return strings.TrimRight(b.String(), "\n")
}
