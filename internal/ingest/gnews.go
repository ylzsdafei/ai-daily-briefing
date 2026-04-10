package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mmcdole/gofeed"

	"briefing-v3/internal/store"
)

// gnewsConfig is the JSON shape stored in Source.ConfigJSON for type
// "google_news". All fields map 1:1 onto the RSS search query string used
// by news.google.com, so Chinese-language briefings can bypass the GFW by
// reading Google News's RSS endpoint directly.
type gnewsConfig struct {
	Query string `json:"query"`
	HL    string `json:"hl"`
	GL    string `json:"gl"`
	CEID  string `json:"ceid"`
	When  string `json:"when"`
}

// gnewsSource queries Google News RSS search. It wraps gofeed in the same
// way rss.go does but constructs the URL on every fetch from the config.
type gnewsSource struct {
	row    *store.Source
	cfg    gnewsConfig
	hc     *http.Client
	parser *gofeed.Parser
}

func newGoogleNewsSource(row *store.Source) (Source, error) {
	var cfg gnewsConfig
	if strings.TrimSpace(row.ConfigJSON) == "" {
		return nil, fmt.Errorf("google_news: empty ConfigJSON for source %d", row.ID)
	}
	if err := json.Unmarshal([]byte(row.ConfigJSON), &cfg); err != nil {
		return nil, fmt.Errorf("google_news: parse ConfigJSON: %w", err)
	}
	if strings.TrimSpace(cfg.Query) == "" {
		return nil, fmt.Errorf("google_news: ConfigJSON.query is required for source %d", row.ID)
	}
	if cfg.HL == "" {
		cfg.HL = "en-US"
	}
	if cfg.GL == "" {
		cfg.GL = "US"
	}
	if cfg.CEID == "" {
		cfg.CEID = "US:en"
	}
	if cfg.When == "" {
		cfg.When = "1d"
	}

	hc := &http.Client{Timeout: 15 * time.Second}
	parser := gofeed.NewParser()
	parser.Client = hc
	parser.UserAgent = "briefing-v3/1.0 (+google_news)"
	return &gnewsSource{
		row:    row,
		cfg:    cfg,
		hc:     hc,
		parser: parser,
	}, nil
}

func (s *gnewsSource) ID() int64    { return s.row.ID }
func (s *gnewsSource) Type() string { return s.row.Type }
func (s *gnewsSource) Name() string { return s.row.Name }

// buildQueryURL assembles the fully escaped Google News RSS search URL.
// Chinese queries MUST be percent-encoded; passing them raw results in
// garbled/empty feeds.
func (s *gnewsSource) buildQueryURL() string {
	q := url.QueryEscape(s.cfg.Query)
	if s.cfg.When != "" {
		q += "+when:" + url.QueryEscape(s.cfg.When)
	}
	return fmt.Sprintf(
		"https://news.google.com/rss/search?q=%s&hl=%s&gl=%s&ceid=%s",
		q,
		url.QueryEscape(s.cfg.HL),
		url.QueryEscape(s.cfg.GL),
		url.QueryEscape(s.cfg.CEID),
	)
}

func (s *gnewsSource) Fetch(ctx context.Context) ([]*store.RawItem, error) {
	feedURL := s.buildQueryURL()
	feed, err := s.parser.ParseURLWithContext(feedURL, ctx)
	if err != nil {
		return nil, fmt.Errorf("google_news: parse %s: %w", feedURL, err)
	}

	now := time.Now().UTC()
	items := make([]*store.RawItem, 0, len(feed.Items))
	for _, fi := range feed.Items {
		if fi == nil {
			continue
		}
		externalID := strings.TrimSpace(fi.GUID)
		if externalID == "" {
			externalID = strings.TrimSpace(fi.Link)
		}
		if externalID == "" {
			continue
		}

		title := strings.TrimSpace(fi.Title)
		if title == "" {
			continue
		}

		published := time.Time{}
		if fi.PublishedParsed != nil {
			published = fi.PublishedParsed.UTC()
		} else if fi.UpdatedParsed != nil {
			published = fi.UpdatedParsed.UTC()
		}
		if published.IsZero() {
			published = now
		}

		author := ""
		if fi.Author != nil {
			author = fi.Author.Name
		}

		content := fi.Description
		if content == "" {
			content = fi.Content
		}

		metaJSON, _ := json.Marshal(map[string]any{
			"query":     s.cfg.Query,
			"hl":        s.cfg.HL,
			"gl":        s.cfg.GL,
			"ceid":      s.cfg.CEID,
			"when":      s.cfg.When,
			"feed_url":  feedURL,
			"source_pub": firstNonEmptyString(feedItemSourceName(fi), ""),
		})

		items = append(items, &store.RawItem{
			DomainID:     s.row.DomainID,
			SourceID:     s.row.ID,
			ExternalID:   externalID,
			URL:          fi.Link,
			Title:        title,
			Author:       author,
			PublishedAt:  published,
			FetchedAt:    now,
			Content:      content,
			MetadataJSON: string(metaJSON),
		})
	}

	if len(items) == 0 {
		return nil, fmt.Errorf("google_news: empty feed for query %q", s.cfg.Query)
	}
	return items, nil
}

// feedItemSourceName returns the publisher name Google News attaches via
// the <source> element, or empty if gofeed did not expose it.
func feedItemSourceName(fi *gofeed.Item) string {
	if fi == nil {
		return ""
	}
	// gofeed exposes the <source> element via Extensions when present.
	if src, ok := fi.Extensions["source"]; ok {
		for _, list := range src {
			for _, ext := range list {
				if v := strings.TrimSpace(ext.Value); v != "" {
					return v
				}
			}
		}
	}
	return ""
}

// firstNonEmptyString mirrors firstNonEmpty (defined in github_trending.go)
// but with an unambiguous name to avoid collisions as the package grows.
func firstNonEmptyString(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func init() {
	Register("google_news", Factory(newGoogleNewsSource))
}
