package main

import (
	"context"
	"fmt"
	"strings"
)

// Two prompts, both about the thing that actually goes wrong when a model uses
// this server: answering from a snippet, and giving up when one engine is
// blocked. Neither is a wrapper around a single tool call - a prompt that only
// says "call web_search" is worth nothing that the tool description does not
// already say.

func (s *Server) listPrompts(ctx context.Context) *ListPromptsResult {
	return &ListPromptsResult{
		Result: s.completeResult(ctx).cacheable(3600000, CacheScopePublic),
		Prompts: []Prompt{
			{
				Name:        "research",
				Title:       "Research a question",
				Description: "Search, read the pages, and answer with citations rather than from snippets.",
				Arguments: []PromptArgument{
					{Name: "question", Description: "What to find out.", Required: true},
					{Name: "depth", Description: "How many sources to read. Defaults to three."},
				},
			},
			{
				Name:        "compare_engines",
				Title:       "Cross-check across engines",
				Description: "Run one query through several engines and report where they disagree.",
				Arguments: []PromptArgument{
					{Name: "query", Description: "What to search for.", Required: true},
				},
			},
		},
	}
}

func (s *Server) getPrompt(ctx context.Context, req *Request) (any, *RPCError) {
	var params struct {
		Name      string            `json:"name"`
		Arguments map[string]string `json:"arguments"`
	}
	if err := req.Bind(&params); err != nil {
		return nil, err
	}

	var description, text string
	switch params.Name {
	case "research":
		question := strings.TrimSpace(params.Arguments["question"])
		if question == "" {
			return nil, Errorf(CodeInvalidParams, "the research prompt needs a question")
		}
		depth := strings.TrimSpace(params.Arguments["depth"])
		if depth == "" {
			depth = "3"
		}
		description = "Research: " + question
		text = fmt.Sprintf(`Find out: %s

Work like this:

1. Search with web_search. If it comes back blocked, that is Google
   rate-limiting this address rather than a failure - call it again with
   engine "duckduckgo" and carry on.

2. Read at least %s of the results with visit_page. Do not answer from the
   snippets. A snippet is chosen by the engine to match the query, which is
   exactly why it can look like it supports a claim the page does not make.

3. Where the sources disagree, say so and say which you believe, rather than
   averaging them into a single confident sentence.

4. Answer with the URLs you actually read. If what you found does not settle
   the question, say that instead of filling the gap.`, question, depth)

	case "compare_engines":
		query := strings.TrimSpace(params.Arguments["query"])
		if query == "" {
			return nil, Errorf(CodeInvalidParams, "the compare_engines prompt needs a query")
		}
		description = "Cross-check: " + query
		text = fmt.Sprintf(`Search for %q on three engines and compare what comes back.

Use web_search three times, with engine "google", "duckduckgo" and "mojeek".
Those three are worth comparing specifically because they are not derived from
each other: Mojeek runs its own crawler, so where it agrees with Google that is
genuine corroboration rather than the same index seen twice.

Then report:
  - the results all three found, which are the ones to trust
  - the results only one found, and which engine found them
  - anything that looks like it is ranking for the words rather than answering
    the question

Available engines:
%s`, query, describeEngines())

	default:
		return nil, Errorf(CodeInvalidParams, "unknown prompt %q", params.Name)
	}

	return &GetPromptResult{
		Result:      s.completeResult(ctx),
		Description: description,
		Messages: []PromptMessage{{
			Role:    "user",
			Content: Content{Type: "text", Text: text},
		}},
	}, nil
}
