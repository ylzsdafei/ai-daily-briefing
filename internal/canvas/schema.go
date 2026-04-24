// Package canvas generates AntV X6 insight-flow diagrams from the daily
// briefing + industry insight. The output JSON is consumed by the Hugo
// frontend shortcode (layouts/shortcodes/insight-canvas.html) which
// renders it inside an X6 Graph with a lightbox for desktop and mobile.
//
// This file defines the Flow JSON schema and its Validate contract.
// Both the Go generator (internal/canvas/generator.go) and the browser
// JS (ai-daily-site-dev/assets/js/insight-canvas.js) agree on this
// shape; any field rename here is a frontend-breaking change.
package canvas

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Flow is the top-level document persisted as IssueInsight.CanvasJSON
// and served to the frontend as /data/canvas/{YYYY-MM-DD}.json.
type Flow struct {
	Title   string     `json:"title"`
	Summary string     `json:"summary"`
	Nodes   []FlowNode `json:"nodes"`
	Edges   []FlowEdge `json:"edges"`
}

// FlowNode is one X6 graph node. Shape hints the frontend renderer
// (hero|rect|rounded|pill); unknown values fall back to a rounded rect.
// X/Y/Width/Height are optional: when absent or zero the frontend runs
// @antv/layout Dagre to auto-compute positions based on data.layer.
// Keeping the fields around lets us stay backward-compatible with any
// on-disk canvas JSON produced before the v1.1 rework while allowing
// the LLM to drop them in fresh generations.
type FlowNode struct {
	ID     string       `json:"id"`
	Shape  string       `json:"shape"`
	X      int          `json:"x,omitempty"`
	Y      int          `json:"y,omitempty"`
	Width  int          `json:"width,omitempty"`
	Height int          `json:"height,omitempty"`
	Label  string       `json:"label"`
	Data   FlowNodeData `json:"data"`
}

// FlowNodeData is the non-visual payload attached to each node.
//
//   - Layer (0-4) is the post-v1.1 rank used for auto layout & color:
//     0=today's headline (single hero node), 1=signal, 2=trend,
//     3=opportunity/risk, 4=action. Required in all new flows.
//   - Tier is the legacy string alias kept for backward compatibility
//     with stored JSON blobs. Validate accepts both — new payloads
//     should set Layer; old ones (Tier only) still pass by mapping.
//   - Description is 2-3 plain-Chinese sentences for a non-technical
//     reader (HR / finance / operations). Highlight marks pivotal
//     nodes (3-5 per flow). SourceURL optionally deep-links the node.
type FlowNodeData struct {
	Tier        string `json:"tier,omitempty"`
	Layer       int    `json:"layer"`
	Description string `json:"description"`
	Highlight   bool   `json:"highlight"`
	SourceURL   string `json:"source_url,omitempty"`
}

// FlowEdge is one directed connection between nodes.
// Style is "solid" (hard causation) or "dashed" (inferred trend link).
type FlowEdge struct {
	ID     string `json:"id"`
	Source string `json:"source"`
	Target string `json:"target"`
	Label  string `json:"label,omitempty"`
	Style  string `json:"style,omitempty"`
}

// Tier constants. Frontend JS reads these as a fallback when Layer is
// missing (i.e. legacy JSON). New flows should populate Layer instead.
const (
	TierSignal      = "signal"
	TierInsight     = "insight"
	TierAction      = "action"
	TierTrend       = "trend"
	TierCompetitor  = "competitor"
	TierHero        = "hero"
	TierOpportunity = "opportunity"
	TierRisk        = "risk"
)

// validTiers is an O(1) lookup used by Validate. Superset of both the
// v1.0 taxonomy (signal/insight/trend/competitor/action) and the v1.1
// taxonomy (hero/signal/trend/opportunity/risk/action) so we accept
// payloads from either generation side-by-side.
var validTiers = map[string]bool{
	TierSignal:      true,
	TierInsight:     true,
	TierAction:      true,
	TierTrend:       true,
	TierCompetitor:  true,
	TierHero:        true,
	TierOpportunity: true,
	TierRisk:        true,
}

// Layer bounds. v1.1 uses a 5-layer Dagre rank (0..4). Anything
// outside that range is treated as a prompt failure.
const (
	MinLayer = 0
	MaxLayer = 4
)

// Node count bounds. Tightened from the v1.0 [15, 30] range because
// user feedback was "太乱、可读性差" — 12-16 layered nodes render
// cleanly at the preview height and stay legible in the lightbox.
const (
	MinNodes = 10
	MaxNodes = 18
)

// Validate enforces the invariants the downstream renderer relies on:
//
//  1. Title and Summary are non-empty (the frontmatter blurb).
//  2. Node count is within [MinNodes, MaxNodes].
//  3. Every node has a recognized Tier (five allowed values).
//  4. Every edge points at two existing node IDs (no dangling refs).
//  5. At least (len(Nodes)-1) edges exist (loose connectivity floor; a
//     stricter DFS-based connectivity check is not needed because the
//     prompt is shaped to produce a layered DAG).
//
// Returns a descriptive error on the first violation so the generator
// can feed it back into the retry prompt.
func (f *Flow) Validate() error {
	if f == nil {
		return errors.New("canvas: flow is nil")
	}
	if f.Title == "" {
		return errors.New("canvas: title is required")
	}
	if f.Summary == "" {
		return errors.New("canvas: summary is required")
	}
	n := len(f.Nodes)
	if n < MinNodes {
		return fmt.Errorf("canvas: need at least %d nodes, got %d", MinNodes, n)
	}
	if n > MaxNodes {
		return fmt.Errorf("canvas: at most %d nodes, got %d", MaxNodes, n)
	}

	ids := make(map[string]bool, n)
	heroCount := 0
	for i, node := range f.Nodes {
		if node.ID == "" {
			return fmt.Errorf("canvas: node[%d] has empty id", i)
		}
		if ids[node.ID] {
			return fmt.Errorf("canvas: duplicate node id %q", node.ID)
		}
		ids[node.ID] = true
		if node.Label == "" {
			return fmt.Errorf("canvas: node[%d] %q has empty label", i, node.ID)
		}
		// Accept a flow that provides *either* a v1.1 Layer *or* a v1.0
		// Tier — but at least one must be present, and if both are set
		// they must be consistent. This lets the frontend render old and
		// new JSON without a migration step.
		hasLayer := node.Data.Layer >= MinLayer && node.Data.Layer <= MaxLayer
		hasTier := node.Data.Tier != ""
		if !hasLayer && !hasTier {
			return fmt.Errorf("canvas: node %q needs either data.layer (0-4) or data.tier", node.ID)
		}
		if hasTier && !validTiers[node.Data.Tier] {
			return fmt.Errorf("canvas: node %q has invalid tier %q (want signal|insight|action|trend|competitor|hero|opportunity|risk)",
				node.ID, node.Data.Tier)
		}
		if node.Data.Layer == 0 && node.Data.Tier == TierHero {
			heroCount++
		}
	}
	// Enforce at most one hero node (layer 0). We don't require exactly
	// one — legacy flows don't have heroes at all and that's fine.
	if heroCount > 1 {
		return fmt.Errorf("canvas: at most one hero/layer-0 node allowed, got %d", heroCount)
	}

	for i, e := range f.Edges {
		if e.ID == "" {
			return fmt.Errorf("canvas: edge[%d] has empty id", i)
		}
		if !ids[e.Source] {
			return fmt.Errorf("canvas: edge %q source %q is not a known node id", e.ID, e.Source)
		}
		if !ids[e.Target] {
			return fmt.Errorf("canvas: edge %q target %q is not a known node id", e.ID, e.Target)
		}
	}

	if want := n - 1; len(f.Edges) < want {
		return fmt.Errorf("canvas: too few edges for a connected flow, got %d want >= %d", len(f.Edges), want)
	}
	return nil
}

// ToJSON returns the canonical JSON encoding used both for DB
// persistence (IssueInsight.CanvasJSON json.RawMessage column) and
// for the /data/canvas/{date}.json file that Hugo serves.
func (f *Flow) ToJSON() (json.RawMessage, error) {
	if f == nil {
		return nil, errors.New("canvas: cannot marshal nil flow")
	}
	buf, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("canvas: marshal flow: %w", err)
	}
	return json.RawMessage(buf), nil
}
