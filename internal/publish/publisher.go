// Package publish defines distribution channel abstractions and implementations.
//
// Each channel has its own file in this package:
//   - slack.go        — Slack webhook (Block Kit payload)
//   - feishu_doc.go   — Feishu wiki doc (year/month/issue hierarchy)
//   - feishu_bot.go   — Feishu group chat message with @all
//
// Publishers are side-effectful but MUST NOT persist Delivery records themselves;
// the main orchestrator records them via Store after observing the returned result.
package publish

import (
	"context"

	"briefing-v3/internal/store"
)

// RenderedIssue bundles an Issue together with its items and insight, ready
// to be formatted by a Publisher for its specific channel.
type RenderedIssue struct {
	Issue   *store.Issue
	Items   []*store.IssueItem // sorted by section, then seq
	Insight *store.IssueInsight
}

// Publisher sends a RenderedIssue to one distribution channel.
type Publisher interface {
	// Name returns the channel tag used in the deliveries table,
	// e.g. "slack_test", "feishu_doc", "feishu_bot".
	Name() string

	// Publish formats and delivers the issue. It MUST return a *store.Delivery
	// reflecting the attempt (including failures), with SentAt populated.
	// A non-nil error indicates an unrecoverable dispatch failure; the
	// returned Delivery.Status should be "failed" in that case.
	Publish(ctx context.Context, rendered *RenderedIssue) (*store.Delivery, error)
}
