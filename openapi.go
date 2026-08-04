package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

// OpenAPI 3.1.0, because that is the revision whose schema dialect is JSON
// Schema 2020-12 - the same dialect MCP tool schemas are written in. That is
// what lets the operation schemas be used verbatim in both places instead of
// being translated, and a translation step is exactly where a spec starts
// describing something the server does not do.
const openAPIVersion = "3.1.0"

// buildSpec renders the OpenAPI document for the operations this server
// serves. It is generated rather than written by hand for the same reason the
// operations are defined once: a hand-written spec is a second description of
// the server that nothing forces to stay true.
func buildSpec(cfg *Config) map[string]any {
	return buildSpecFor(cfg, cfg.BaseURL())
}

// buildSpecFor renders the document with an explicit server URL, so that a
// request that arrived through a proxy can be answered with a spec naming the
// origin the caller actually used rather than the address this process listens
// on.
func buildSpecFor(cfg *Config, baseURL string) map[string]any {
	paths := map[string]any{}
	base := cfg.API.BasePath

	for _, op := range Operations() {
		if op.Path == "" {
			continue
		}
		paths[base+op.Path] = map[string]any{
			"post": map[string]any{
				"operationId": op.Name,
				"summary":     op.Summary,
				"description": op.Description,
				"tags":        []string{tagFor(op)},
				"requestBody": map[string]any{
					"required": true,
					"content": map[string]any{
						"application/json": map[string]any{"schema": op.Schema},
					},
				},
				"responses": map[string]any{
					"200": jsonResponse("The result.", map[string]any{"type": "object"}),
					"400": errorResponse("The request could not be acted on: a missing field, " +
						"an unknown engine, a URL that is not one."),
					"401": errorResponse("A bearer token is required and was missing or wrong."),
					"502": errorResponse("The upstream site could not be scraped: it was " +
						"unreachable, or it served a bot check instead of results."),
				},
			},
		}
	}

	// The index and the health check are part of the contract too: a client
	// that has the spec should not have to be told separately that they exist.
	paths[base] = map[string]any{
		"get": map[string]any{
			"operationId": "listOperations",
			"summary":     "List the operations this server serves.",
			"description": "Returns every operation with its path and input schema. " +
				"The same information as the OpenAPI document, in the shape a client " +
				"that just wants to enumerate them can use directly.",
			"tags":      []string{"Meta"},
			"responses": map[string]any{"200": jsonResponse("The operation list.", map[string]any{"type": "object"})},
		},
	}
	paths[base+"/health"] = map[string]any{
		"get": map[string]any{
			"operationId": "health",
			"summary":     "Report whether the server is up and whether the browser is running.",
			"description": "Answers without starting the browser, so it is safe to poll. " +
				"A browser that is not running is not a fault: it starts on the first " +
				"call that needs it and closes again when the server goes idle.",
			"tags":      []string{"Meta"},
			"responses": map[string]any{"200": jsonResponse("The status.", map[string]any{"type": "object"})},
		},
	}

	spec := map[string]any{
		"openapi": openAPIVersion,
		"info": map[string]any{
			"title":       "noapi-search",
			"version":     cfg.Server.Version,
			"description": specDescription(cfg),
			"license":     map[string]any{"name": "MIT"},
		},
		"servers": []any{map[string]any{
			"url":         baseURL,
			"description": "This server.",
		}},
		"tags":  specTags(),
		"paths": paths,
		"components": map[string]any{
			"schemas": map[string]any{
				"Error": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"error": map[string]any{
							"type":        "string",
							"description": "What went wrong, in the words a caller can act on.",
						},
						"operation": map[string]any{"type": "string"},
					},
					"required": []string{"error"},
				},
			},
		},
	}

	if token := cfg.Server.AuthTokenValue(); token != "" {
		spec["components"].(map[string]any)["securitySchemes"] = map[string]any{
			"bearerAuth": map[string]any{
				"type":        "http",
				"scheme":      "bearer",
				"description": "The token configured as server.auth_token.",
			},
		}
		spec["security"] = []any{map[string]any{"bearerAuth": []any{}}}
	}
	return spec
}

// specDescription is the prose at the top of the document. It says what a
// reader needs before their first call rather than restating the title.
func specDescription(cfg *Config) string {
	var b strings.Builder
	b.WriteString("Real search results and web content, with no API keys: every operation " +
		"here drives a headless browser or reads a search engine's plain HTML endpoint.\n\n")
	b.WriteString("These are the same capabilities this process serves over the Model " +
		"Context Protocol at `" + firstPath(cfg) + "`, defined once and exposed both ways. " +
		"An MCP client should use that endpoint; this one is for everything else.\n\n")
	b.WriteString("Two things worth knowing before you build on it. Scraping is " +
		"rate-limited: an operation answering 502 with a bot-check message is not a " +
		"fault to retry immediately, and the `duckduckgo`, `bing` and `mojeek` engines " +
		"need no browser and are rarely blocked. And every result is shaped by what " +
		"the page looked like at the time, so a field that comes back empty means it " +
		"was not on the page - not that it does not exist.\n\n")
	if cfg.Server.AuthTokenValue() != "" {
		b.WriteString("Every request needs `Authorization: Bearer <token>`.\n")
	} else {
		b.WriteString("This server is running without an auth token, so every request is " +
			"accepted. Set `server.auth_token` before exposing it beyond this machine.\n")
	}
	return b.String()
}

func firstPath(cfg *Config) string {
	if endpoints, err := cfg.Endpoints(); err == nil && len(endpoints) > 0 {
		return endpoints[0].Path
	}
	return "/mcp"
}

// specTags groups the operations in the way a reader of the documentation
// wants them: by what they are for, not by which page they scrape.
func specTags() []any {
	return []any{
		map[string]any{"name": "Search", "description": "General web search, across several engines."},
		map[string]any{"name": "Verticals", "description": "Google's specialised searches: news, scholar, books, shopping, images."},
		map[string]any{"name": "Places", "description": "Geocoding, place search and directions."},
		map[string]any{"name": "Answers", "description": "Google's answer cards: weather, finance, translation, trends, travel."},
		map[string]any{"name": "Content", "description": "Reading pages and images."},
		map[string]any{"name": "Meta", "description": "Discovery and health."},
	}
}

// tagFor groups one operation. It is a switch rather than a field on the
// operation because the grouping is a property of the documentation, not of the
// capability, and it changes when the documentation is reorganised.
func tagFor(op *Operation) string {
	switch op.Name {
	case "web_search", "google_search":
		return "Search"
	case "google_news", "google_scholar", "google_books", "google_shopping", "google_images":
		return "Verticals"
	case "geocode", "google_maps", "google_maps_directions":
		return "Places"
	case "google_weather", "google_finance", "google_translate", "google_trends",
		"google_flights", "google_hotels":
		return "Answers"
	default:
		return "Content"
	}
}

func jsonResponse(description string, schema map[string]any) map[string]any {
	return map[string]any{
		"description": description,
		"content": map[string]any{
			"application/json": map[string]any{"schema": schema},
		},
	}
}

func errorResponse(description string) map[string]any {
	return map[string]any{
		"description": description,
		"content": map[string]any{
			"application/json": map[string]any{
				"schema": map[string]any{"$ref": "#/components/schemas/Error"},
			},
		},
	}
}

// specJSON renders the document. It is built once at startup and served from
// memory: nothing in it changes while the process runs, and rebuilding it per
// request would be work done to produce an identical answer.
func specJSON(cfg *Config) ([]byte, error) {
	return specJSONFor(cfg, cfg.BaseURL())
}

// specJSONFor renders the document with an explicit server URL.
func specJSONFor(cfg *Config, baseURL string) ([]byte, error) {
	data, err := json.MarshalIndent(buildSpecFor(cfg, baseURL), "", "  ")
	if err != nil {
		return nil, fmt.Errorf("building the OpenAPI document: %w", err)
	}
	return data, nil
}
