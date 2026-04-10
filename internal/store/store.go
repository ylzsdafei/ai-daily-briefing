package store

import (
	"context"
	"time"
)

// Store is the data access interface. Concrete implementation must be
// injection-friendly and context-aware. Errors should be wrapped with
// meaningful context so callers can distinguish connection errors from
// not-found vs constraint violations.
//
// Day 1 has a single SQLite implementation. Schema is initialized via Migrate().
type Store interface {
	// Lifecycle
	Migrate(ctx context.Context) error
	Close() error

	// Domain
	UpsertDomain(ctx context.Context, d *Domain) error
	GetDomain(ctx context.Context, id string) (*Domain, error)

	// Source
	UpsertSource(ctx context.Context, s *Source) (int64, error)
	ListEnabledSources(ctx context.Context, domainID string) ([]*Source, error)

	// RawItem
	// InsertRawItems inserts items in bulk; duplicates (source_id, external_id)
	// must be silently skipped (ON CONFLICT DO NOTHING).
	InsertRawItems(ctx context.Context, items []*RawItem) error
	ListRecentRawItems(ctx context.Context, domainID string, since time.Time) ([]*RawItem, error)
	UpdateRawItemContent(ctx context.Context, id int64, content string) error

	// Issue
	// UpsertIssue inserts or updates an issue for (domain_id, issue_date),
	// returning the resulting id.
	UpsertIssue(ctx context.Context, issue *Issue) (int64, error)
	GetIssueByDate(ctx context.Context, domainID string, date time.Time) (*Issue, error)
	MarkIssuePublished(ctx context.Context, issueID int64) error
	NextIssueNumber(ctx context.Context, domainID string) (int, error)

	// IssueItem
	// ReplaceIssueItems deletes existing items for the issue and inserts
	// the provided set atomically. Used by compose to rewrite issue items
	// on retry.
	ReplaceIssueItems(ctx context.Context, issueID int64, items []*IssueItem) error
	ListIssueItems(ctx context.Context, issueID int64) ([]*IssueItem, error)

	// IssueInsight
	UpsertIssueInsight(ctx context.Context, insight *IssueInsight) error
	GetIssueInsight(ctx context.Context, issueID int64) (*IssueInsight, error)

	// Delivery
	InsertDelivery(ctx context.Context, delivery *Delivery) error
	ListDeliveries(ctx context.Context, issueID int64) ([]*Delivery, error)
}
