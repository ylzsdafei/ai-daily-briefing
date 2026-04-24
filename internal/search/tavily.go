// Package search wraps web-search providers so other components
// (currently only canvas.Generator) can treat "get me the latest on
// topic X" as a simple function call.
//
// Tavily (https://tavily.com) is the default provider because its
// API is purpose-built for LLM agents: it returns a short `answer`
// field (already summarized) + up to N `results` with title/url/
// snippet, all as structured JSON. No HTML parsing, no rate-limit
// gymnastics. The free tier is 1000 searches/month which is more
// than enough for one daily canvas regeneration.
package search

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// TavilyClient is safe for concurrent use.
type TavilyClient struct {
	apiKey     string
	httpClient *http.Client
	endpoint   string
	timeout    time.Duration
}

// NewTavilyClient constructs a client with sensible defaults.
// Pass "" to apiKey to trigger a clear error on first Search rather
// than producing ambiguous 401s from the API.
func NewTavilyClient(apiKey string) *TavilyClient {
	return &TavilyClient{
		apiKey:     strings.TrimSpace(apiKey),
		httpClient: &http.Client{},
		endpoint:   "https://api.tavily.com/search",
		timeout:    20 * time.Second,
	}
}

// WithTimeout returns a copy with the given per-request timeout.
func (c *TavilyClient) WithTimeout(d time.Duration) *TavilyClient {
	cp := *c
	cp.timeout = d
	return &cp
}

// Result is a single search hit. Content is Tavily's own snippet
// (typically the first ~200-500 chars of the page, already cleaned).
type Result struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
	Score   float64 `json:"score,omitempty"`
}

// Response wraps the full Tavily payload. Answer is a single-paragraph
// summary Tavily auto-writes from the top results; prefer it when
// cramming search output into an LLM context window.
type Response struct {
	Query   string   `json:"query"`
	Answer  string   `json:"answer"`
	Results []Result `json:"results"`
}

// Opts tunes one search call.
type Opts struct {
	// MaxResults is the number of hits to return. 0 => provider default (5).
	MaxResults int
	// IncludeAnswer asks Tavily to summarize the top hits into a single
	// short paragraph. Default true (LLM-friendly).
	IncludeAnswer bool
	// SearchDepth is "basic" (fast, free tier) or "advanced" (slower,
	// consumes more of the monthly quota). Default basic.
	SearchDepth string
}

// Search executes a single query and returns up to MaxResults hits
// plus Tavily's auto-generated answer paragraph.
func (c *TavilyClient) Search(ctx context.Context, query string, opts Opts) (*Response, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, errors.New("search: query is empty")
	}
	if c.apiKey == "" {
		return nil, errors.New("search: TAVILY_API_KEY is empty")
	}

	if opts.MaxResults <= 0 {
		opts.MaxResults = 5
	}
	if opts.SearchDepth == "" {
		opts.SearchDepth = "basic"
	}

	payload := map[string]any{
		"api_key":        c.apiKey,
		"query":          query,
		"max_results":    opts.MaxResults,
		"include_answer": opts.IncludeAnswer,
		"search_depth":   opts.SearchDepth,
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal: %w", err)
	}

	reqCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, "POST", c.endpoint, bytes.NewReader(buf))
	if err != nil {
		return nil, fmt.Errorf("new request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(body)
		if len(snippet) > 500 {
			snippet = snippet[:500]
		}
		return nil, fmt.Errorf("tavily http %d: %s", resp.StatusCode, snippet)
	}

	var out Response
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("parse tavily response: %w", err)
	}
	return &out, nil
}

// FormatForLLM renders a Response into a compact markdown blob that
// can be dropped into a tool-call reply inside the conversation.
// Keeps answer + top-N hits title/url/snippet.
func (r *Response) FormatForLLM() string {
	if r == nil {
		return "(no results)"
	}
	var b strings.Builder
	if strings.TrimSpace(r.Answer) != "" {
		b.WriteString("Answer: ")
		b.WriteString(r.Answer)
		b.WriteString("\n\n")
	}
	if len(r.Results) == 0 {
		b.WriteString("(no results)")
		return b.String()
	}
	b.WriteString("Top results:\n")
	for i, res := range r.Results {
		fmt.Fprintf(&b, "%d. %s\n   URL: %s\n   %s\n",
			i+1,
			strings.TrimSpace(res.Title),
			strings.TrimSpace(res.URL),
			strings.TrimSpace(res.Content),
		)
	}
	return b.String()
}
