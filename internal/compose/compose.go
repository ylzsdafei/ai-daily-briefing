// Package compose stitches classified RawItems plus LLM-generated
// section markdown into the final []*store.IssueItem used by the
// persistence and render layers.
//
// Pipeline position:
//
//	rank → classify → compose → render
//
// compose is the first stage that crosses back into the store.IssueItem
// shape. It does NOT persist anything — callers (cmd/briefing) must
// pass the returned IssueItems to store.Store.InsertIssueItems.
package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"briefing-v3/internal/generate"
	"briefing-v3/internal/store"
)

// SectionConfig mirrors the 'sections' block of config/ai.yaml. Only the
// fields compose actually consumes are declared here to keep compose
// decoupled from the YAML loader.
type SectionConfig struct {
	ID       string
	Title    string
	MinItems int
	MaxItems int
}

// Composer is the public interface of this package.
type Composer interface {
	// Compose turns the classified buckets into ordered IssueItems
	// ready for insertion. issueID is set on every returned IssueItem.
	// sectioned is the output of classify.Classifier.Classify.
	// sections is the ordered list of section configs (typically read
	// from config/ai.yaml).
	// summarizer is the LLM text generator from the generate package;
	// a nil summarizer returns an error (compose refuses to make up
	// body text without an LLM).
	Compose(
		ctx context.Context,
		issueID int64,
		sectioned map[string][]*store.RawItem,
		sections []SectionConfig,
		summarizer generate.Summarizer,
	) ([]*store.IssueItem, error)
}

// New returns a default Composer. It is stateless so a single instance
// can be shared across goroutines.
func New() Composer {
	return &composer{}
}

type composer struct{}

// Compose walks sections in order, caps each bucket at MaxItems, asks
// the Summarizer to produce the section's markdown body, then splits
// that markdown into one IssueItem per numbered entry.
func (c *composer) Compose(
	ctx context.Context,
	issueID int64,
	sectioned map[string][]*store.RawItem,
	sections []SectionConfig,
	summarizer generate.Summarizer,
) ([]*store.IssueItem, error) {
	if summarizer == nil {
		return nil, errors.New("compose: Summarizer is required")
	}
	if len(sections) == 0 {
		return nil, errors.New("compose: no sections configured")
	}

	var out []*store.IssueItem

	for _, sec := range sections {
		items := sectioned[sec.ID]
		if len(items) == 0 {
			continue
		}

		// Cap at MaxItems. The items within a section are already
		// ranked by score (the classify step preserved rank order), so
		// simply taking the prefix keeps the strongest entries.
		if sec.MaxItems > 0 && len(items) > sec.MaxItems {
			items = items[:sec.MaxItems]
		}

		md, err := summarizer.Summarize(ctx, sec.Title, items)
		if err != nil {
			return nil, fmt.Errorf("compose: summarize section %q: %w", sec.ID, err)
		}
		md = strings.TrimSpace(md)
		if md == "" {
			continue
		}

		issueItems := splitMarkdownIntoIssueItems(md, sec.ID, issueID, items)
		out = append(out, issueItems...)
	}

	return out, nil
}

// numberedEntryStart matches "1. " at the beginning of a line, which is
// upstream's canonical entry delimiter for Step 1 summaries.
var numberedEntryStart = regexp.MustCompile(`(?m)^\s*(\d+)\.\s+`)

// splitMarkdownIntoIssueItems slices the LLM output into one IssueItem
// per "1. ", "2. ", ... entry. The first bolded fragment on each entry
// becomes the IssueItem.Title; the entire chunk becomes BodyMD.
//
// rawItems is the section's candidate items — used to populate the
// SourceURLsJSON / RawItemIDsJSON columns. We attach every candidate
// id/url to every produced IssueItem rather than trying to guess which
// ids the LLM quoted, because:
//
//  1. The LLM prompt allows mixing multiple candidates per entry, and
//  2. Downstream publishers only use these fields as evidence anchors,
//     not as a strict 1:1 join.
//
// This conservative mapping can be tightened in a later revision by
// parsing the (briefing) anchors out of the body markdown.
func splitMarkdownIntoIssueItems(md string, sectionID string, issueID int64, rawItems []*store.RawItem) []*store.IssueItem {
	// Pre-compute the source JSON blobs once per section; every
	// IssueItem in this section references the same candidate set.
	srcURLs := make([]string, 0, len(rawItems))
	srcIDs := make([]int64, 0, len(rawItems))
	for _, it := range rawItems {
		if it == nil {
			continue
		}
		if it.URL != "" {
			srcURLs = append(srcURLs, it.URL)
		}
		srcIDs = append(srcIDs, it.ID)
	}
	urlsJSON, _ := json.Marshal(srcURLs)
	idsJSON, _ := json.Marshal(srcIDs)

	// Find all "N. " offsets. If none, treat the whole blob as a single
	// anonymous entry (this is the degraded case where the LLM returned
	// prose instead of a list).
	starts := numberedEntryStart.FindAllStringIndex(md, -1)
	if len(starts) == 0 {
		return []*store.IssueItem{
			{
				IssueID:        issueID,
				Section:        sectionID,
				Seq:            1,
				Title:          extractTitle(md),
				BodyMD:         md,
				SourceURLsJSON: string(urlsJSON),
				RawItemIDsJSON: string(idsJSON),
			},
		}
	}

	items := make([]*store.IssueItem, 0, len(starts))
	for i, loc := range starts {
		begin := loc[0]
		var end int
		if i+1 < len(starts) {
			end = starts[i+1][0]
		} else {
			end = len(md)
		}
		chunk := strings.TrimSpace(md[begin:end])
		if chunk == "" {
			continue
		}
		title := extractTitle(chunk)
		items = append(items, &store.IssueItem{
			IssueID:        issueID,
			Section:        sectionID,
			Seq:            i + 1,
			Title:          title,
			BodyMD:         chunk,
			SourceURLsJSON: string(urlsJSON),
			RawItemIDsJSON: string(idsJSON),
		})
	}
	return items
}

// titleBoldRegex pulls the first **bolded** fragment out of an entry.
// Upstream's Step 1 prompt mandates "1. **title.** body" so this is a
// reliable extraction point.
var titleBoldRegex = regexp.MustCompile(`\*\*([^*]+?)\*\*`)

// titleFirstLineTrim strips leading "N. " and surrounding whitespace.
var titleFirstLineTrim = regexp.MustCompile(`^\s*\d+\.\s*`)

// extractTitle finds the entry's headline. Preference order:
//
//  1. First **bolded** fragment — the upstream canonical format.
//  2. The first non-empty line with its leading number stripped.
//  3. Empty string (caller should substitute a placeholder).
func extractTitle(chunk string) string {
	if m := titleBoldRegex.FindStringSubmatch(chunk); len(m) >= 2 {
		t := strings.TrimSpace(m[1])
		t = strings.TrimRight(t, "。.!?!?")
		if t != "" {
			return t
		}
	}

	for _, line := range strings.Split(chunk, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		trimmed = titleFirstLineTrim.ReplaceAllString(trimmed, "")
		trimmed = strings.Trim(trimmed, "*_` 　")
		if trimmed != "" {
			// Cap absurdly long lines so the column is usable.
			if rs := []rune(trimmed); len(rs) > 120 {
				trimmed = string(rs[:120])
			}
			return trimmed
		}
	}
	return ""
}
