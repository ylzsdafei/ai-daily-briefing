package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// initialSchema is embedded from internal/store/migrations/001_initial.sql.
// A mirror copy lives at migrations/001_initial.sql (project root) for
// external migration tools and DBAs to inspect; the two files must stay in sync.
//
//go:embed migrations/001_initial.sql
var initialSchema string

//go:embed migrations/002_weekly.sql
var weeklySchema string

// New opens (or creates) a SQLite database at dbPath and returns a Store.
// The caller must invoke Migrate(ctx) before using the Store for reads/writes.
func New(dbPath string) (Store, error) {
	// modernc.org/sqlite driver name is "sqlite".
	// Pragmas: busy_timeout avoids "database is locked" during concurrent writes,
	// foreign_keys enforces referential integrity, journal_mode=WAL improves concurrency.
	dsn := dbPath + "?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("sqlite open: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("sqlite ping: %w", err)
	}
	return &sqliteStore{db: db}, nil
}

type sqliteStore struct {
	db *sql.DB
}

// -------- Lifecycle --------

func (s *sqliteStore) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, initialSchema); err != nil {
		return fmt.Errorf("migrate 001: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, weeklySchema); err != nil {
		return fmt.Errorf("migrate 002: %w", err)
	}
	return nil
}

func (s *sqliteStore) Close() error {
	return s.db.Close()
}

// -------- Domain --------

func (s *sqliteStore) UpsertDomain(ctx context.Context, d *Domain) error {
	const q = `
		INSERT INTO domains (id, name, config_path, created_at)
		VALUES (?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP))
		ON CONFLICT(id) DO UPDATE SET
			name = excluded.name,
			config_path = excluded.config_path
	`
	var createdAt any
	if !d.CreatedAt.IsZero() {
		createdAt = d.CreatedAt
	}
	if _, err := s.db.ExecContext(ctx, q, d.ID, d.Name, d.ConfigPath, createdAt); err != nil {
		return fmt.Errorf("upsert domain %q: %w", d.ID, err)
	}
	return nil
}

func (s *sqliteStore) GetDomain(ctx context.Context, id string) (*Domain, error) {
	const q = `SELECT id, name, COALESCE(config_path, ''), created_at FROM domains WHERE id = ?`
	var d Domain
	err := s.db.QueryRowContext(ctx, q, id).Scan(&d.ID, &d.Name, &d.ConfigPath, &d.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get domain %q: %w", id, err)
	}
	return &d, nil
}

// -------- Source --------

func (s *sqliteStore) UpsertSource(ctx context.Context, src *Source) (int64, error) {
	const q = `
		INSERT INTO sources (domain_id, type, name, config_json, enabled, created_at)
		VALUES (?, ?, ?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP))
		ON CONFLICT(domain_id, type, name) DO UPDATE SET
			config_json = excluded.config_json,
			enabled = excluded.enabled
		RETURNING id
	`
	var createdAt any
	if !src.CreatedAt.IsZero() {
		createdAt = src.CreatedAt
	}
	enabled := 0
	if src.Enabled {
		enabled = 1
	}
	var id int64
	err := s.db.QueryRowContext(ctx, q,
		src.DomainID, src.Type, src.Name, src.ConfigJSON, enabled, createdAt,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert source %s/%s/%s: %w", src.DomainID, src.Type, src.Name, err)
	}
	return id, nil
}

func (s *sqliteStore) ListEnabledSources(ctx context.Context, domainID string) ([]*Source, error) {
	const q = `
		SELECT id, domain_id, type, name, config_json, enabled, created_at
		FROM sources
		WHERE domain_id = ? AND enabled = 1
		ORDER BY id
	`
	rows, err := s.db.QueryContext(ctx, q, domainID)
	if err != nil {
		return nil, fmt.Errorf("list sources %q: %w", domainID, err)
	}
	defer rows.Close()

	var out []*Source
	for rows.Next() {
		var src Source
		var enabled int
		if err := rows.Scan(
			&src.ID, &src.DomainID, &src.Type, &src.Name,
			&src.ConfigJSON, &enabled, &src.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan source: %w", err)
		}
		src.Enabled = enabled != 0
		// Cheap one-off parse of config_json to surface the Category field.
		// We intentionally ignore errors here — a missing or malformed JSON
		// leaves Category empty, which downstream rule-based classify will
		// treat as "unknown → fall through to LLM".
		src.Category = extractSourceCategory(src.ConfigJSON)
		out = append(out, &src)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate sources: %w", err)
	}
	return out, nil
}

// extractSourceCategory pulls the "category" string out of a source's
// config_json blob. Returns an empty string on any parse error or if the
// field is missing. Kept here (not in types.go) because it is a SQLite
// implementation detail of how sources.config_json is serialized by
// cmd/briefing/main.go:marshalSourceConfig.
func extractSourceCategory(configJSON string) string {
	if strings.TrimSpace(configJSON) == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(configJSON), &m); err != nil {
		return ""
	}
	v, ok := m["category"]
	if !ok {
		return ""
	}
	cat, ok := v.(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(cat)
}

// -------- RawItem --------

func (s *sqliteStore) InsertRawItems(ctx context.Context, items []*RawItem) error {
	if len(items) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	const q = `
		INSERT OR IGNORE INTO raw_items
			(domain_id, source_id, external_id, url, title, author, published_at, fetched_at, content, metadata_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`
	stmt, err := tx.PrepareContext(ctx, q)
	if err != nil {
		return fmt.Errorf("prepare insert raw_items: %w", err)
	}
	defer stmt.Close()

	for _, it := range items {
		var publishedAt any
		if !it.PublishedAt.IsZero() {
			publishedAt = it.PublishedAt
		}
		var fetchedAt any
		if !it.FetchedAt.IsZero() {
			fetchedAt = it.FetchedAt
		} else {
			fetchedAt = time.Now().UTC()
		}
		if _, err := stmt.ExecContext(ctx,
			it.DomainID, it.SourceID, nullString(it.ExternalID),
			it.URL, it.Title, it.Author,
			publishedAt, fetchedAt,
			it.Content, it.MetadataJSON,
		); err != nil {
			return fmt.Errorf("insert raw_item (source=%d ext=%q): %w", it.SourceID, it.ExternalID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit insert raw_items: %w", err)
	}
	return nil
}

func (s *sqliteStore) ListRecentRawItems(ctx context.Context, domainID string, since time.Time) ([]*RawItem, error) {
	const q = `
		SELECT id, domain_id, source_id, COALESCE(external_id, ''), url,
		       COALESCE(title, ''), COALESCE(author, ''),
		       published_at, fetched_at,
		       COALESCE(content, ''), COALESCE(metadata_json, '')
		FROM raw_items
		WHERE domain_id = ? AND fetched_at >= ?
		ORDER BY fetched_at DESC, id DESC
	`
	rows, err := s.db.QueryContext(ctx, q, domainID, since)
	if err != nil {
		return nil, fmt.Errorf("list raw_items %q: %w", domainID, err)
	}
	defer rows.Close()

	var out []*RawItem
	for rows.Next() {
		var it RawItem
		var publishedAt sql.NullTime
		if err := rows.Scan(
			&it.ID, &it.DomainID, &it.SourceID, &it.ExternalID, &it.URL,
			&it.Title, &it.Author,
			&publishedAt, &it.FetchedAt,
			&it.Content, &it.MetadataJSON,
		); err != nil {
			return nil, fmt.Errorf("scan raw_item: %w", err)
		}
		if publishedAt.Valid {
			it.PublishedAt = publishedAt.Time
		}
		out = append(out, &it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate raw_items: %w", err)
	}
	return out, nil
}

func (s *sqliteStore) UpdateRawItemContent(ctx context.Context, id int64, content string) error {
	const q = `UPDATE raw_items SET content = ? WHERE id = ?`
	res, err := s.db.ExecContext(ctx, q, content, id)
	if err != nil {
		return fmt.Errorf("update raw_item %d content: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("update raw_item %d: not found", id)
	}
	return nil
}

// -------- Issue --------

func (s *sqliteStore) UpsertIssue(ctx context.Context, issue *Issue) (int64, error) {
	// Use ON CONFLICT to preserve id on update (INSERT OR REPLACE would reassign id
	// and cascade-break FK references from issue_items).
	const q = `
		INSERT INTO issues
			(domain_id, issue_date, issue_number, title, summary, status,
			 source_count, item_count, generated_at, published_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(domain_id, issue_date) DO UPDATE SET
			issue_number = excluded.issue_number,
			title = excluded.title,
			summary = excluded.summary,
			status = excluded.status,
			source_count = excluded.source_count,
			item_count = excluded.item_count,
			generated_at = excluded.generated_at,
			published_at = excluded.published_at
		RETURNING id
	`
	status := issue.Status
	if status == "" {
		status = IssueStatusDraft
	}
	var id int64
	err := s.db.QueryRowContext(ctx, q,
		issue.DomainID, issue.IssueDate.Format("2006-01-02"),
		nullInt(int64(issue.IssueNumber)),
		issue.Title, issue.Summary, status,
		issue.SourceCount, issue.ItemCount,
		nullTimePtr(issue.GeneratedAt), nullTimePtr(issue.PublishedAt),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert issue %s/%s: %w", issue.DomainID, issue.IssueDate.Format("2006-01-02"), err)
	}
	return id, nil
}

func (s *sqliteStore) GetIssueByDate(ctx context.Context, domainID string, date time.Time) (*Issue, error) {
	const q = `
		SELECT id, domain_id, issue_date, COALESCE(issue_number, 0),
		       COALESCE(title, ''), COALESCE(summary, ''), status,
		       COALESCE(source_count, 0), COALESCE(item_count, 0),
		       generated_at, published_at
		FROM issues
		WHERE domain_id = ? AND issue_date = ?
	`
	row := s.db.QueryRowContext(ctx, q, domainID, date.Format("2006-01-02"))

	var is Issue
	var generatedAt, publishedAt sql.NullTime
	err := row.Scan(
		&is.ID, &is.DomainID, &is.IssueDate, &is.IssueNumber,
		&is.Title, &is.Summary, &is.Status,
		&is.SourceCount, &is.ItemCount,
		&generatedAt, &publishedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get issue %s/%s: %w", domainID, date.Format("2006-01-02"), err)
	}
	if generatedAt.Valid {
		t := generatedAt.Time
		is.GeneratedAt = &t
	}
	if publishedAt.Valid {
		t := publishedAt.Time
		is.PublishedAt = &t
	}
	return &is, nil
}

func (s *sqliteStore) MarkIssuePublished(ctx context.Context, issueID int64) error {
	const q = `
		UPDATE issues
		SET status = ?, published_at = COALESCE(published_at, CURRENT_TIMESTAMP)
		WHERE id = ?
	`
	res, err := s.db.ExecContext(ctx, q, IssueStatusPublished, issueID)
	if err != nil {
		return fmt.Errorf("mark issue %d published: %w", issueID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("mark issue %d published: not found", issueID)
	}
	return nil
}

func (s *sqliteStore) NextIssueNumber(ctx context.Context, domainID string) (int, error) {
	const q = `SELECT COALESCE(MAX(issue_number), 0) + 1 FROM issues WHERE domain_id = ?`
	var n int
	if err := s.db.QueryRowContext(ctx, q, domainID).Scan(&n); err != nil {
		return 0, fmt.Errorf("next issue number %q: %w", domainID, err)
	}
	return n, nil
}

// -------- IssueItem --------

func (s *sqliteStore) ReplaceIssueItems(ctx context.Context, issueID int64, items []*IssueItem) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `DELETE FROM issue_items WHERE issue_id = ?`, issueID); err != nil {
		return fmt.Errorf("delete issue_items %d: %w", issueID, err)
	}

	if len(items) > 0 {
		const q = `
			INSERT INTO issue_items
				(issue_id, section, seq, title, body_md, source_urls_json, raw_item_ids_json, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP))
		`
		stmt, err := tx.PrepareContext(ctx, q)
		if err != nil {
			return fmt.Errorf("prepare insert issue_items: %w", err)
		}
		defer stmt.Close()
		for _, it := range items {
			var createdAt any
			if !it.CreatedAt.IsZero() {
				createdAt = it.CreatedAt
			}
			if _, err := stmt.ExecContext(ctx,
				issueID, it.Section, it.Seq, it.Title, it.BodyMD,
				it.SourceURLsJSON, it.RawItemIDsJSON, createdAt,
			); err != nil {
				return fmt.Errorf("insert issue_item (issue=%d section=%s seq=%d): %w",
					issueID, it.Section, it.Seq, err)
			}
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit replace issue_items: %w", err)
	}
	return nil
}

func (s *sqliteStore) ListIssueItems(ctx context.Context, issueID int64) ([]*IssueItem, error) {
	const q = `
		SELECT id, issue_id, section, seq, title, body_md,
		       COALESCE(source_urls_json, ''), COALESCE(raw_item_ids_json, ''),
		       created_at
		FROM issue_items
		WHERE issue_id = ?
		ORDER BY section, seq, id
	`
	rows, err := s.db.QueryContext(ctx, q, issueID)
	if err != nil {
		return nil, fmt.Errorf("list issue_items %d: %w", issueID, err)
	}
	defer rows.Close()

	var out []*IssueItem
	for rows.Next() {
		var it IssueItem
		if err := rows.Scan(
			&it.ID, &it.IssueID, &it.Section, &it.Seq, &it.Title, &it.BodyMD,
			&it.SourceURLsJSON, &it.RawItemIDsJSON, &it.CreatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan issue_item: %w", err)
		}
		out = append(out, &it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate issue_items: %w", err)
	}
	return out, nil
}

// -------- IssueInsight --------

func (s *sqliteStore) UpsertIssueInsight(ctx context.Context, insight *IssueInsight) error {
	const q = `
		INSERT INTO issue_insights
			(issue_id, industry_md, our_md, model, temperature, retry_count, generated_at)
		VALUES (?, ?, ?, ?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP))
		ON CONFLICT(issue_id) DO UPDATE SET
			industry_md = excluded.industry_md,
			our_md = excluded.our_md,
			model = excluded.model,
			temperature = excluded.temperature,
			retry_count = excluded.retry_count,
			generated_at = excluded.generated_at
	`
	var generatedAt any
	if !insight.GeneratedAt.IsZero() {
		generatedAt = insight.GeneratedAt
	}
	if _, err := s.db.ExecContext(ctx, q,
		insight.IssueID, insight.IndustryMD, insight.OurMD,
		insight.Model, insight.Temperature, insight.RetryCount, generatedAt,
	); err != nil {
		return fmt.Errorf("upsert insight for issue %d: %w", insight.IssueID, err)
	}
	return nil
}

func (s *sqliteStore) GetIssueInsight(ctx context.Context, issueID int64) (*IssueInsight, error) {
	const q = `
		SELECT id, issue_id,
		       COALESCE(industry_md, ''), COALESCE(our_md, ''),
		       COALESCE(model, ''), COALESCE(temperature, 0),
		       retry_count, generated_at
		FROM issue_insights
		WHERE issue_id = ?
	`
	var in IssueInsight
	err := s.db.QueryRowContext(ctx, q, issueID).Scan(
		&in.ID, &in.IssueID, &in.IndustryMD, &in.OurMD,
		&in.Model, &in.Temperature, &in.RetryCount, &in.GeneratedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get insight for issue %d: %w", issueID, err)
	}
	return &in, nil
}

// -------- WeeklyIssue --------

func (s *sqliteStore) UpsertWeeklyIssue(ctx context.Context, w *WeeklyIssue) (int64, error) {
	const q = `
		INSERT INTO weekly_issues
			(domain_id, year, week, start_date, end_date, title,
			 focus_md, signals_md, trends_md, takeaways_md, ponder_md,
			 full_md, daily_issue_ids, status, generated_at, published_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(domain_id, year, week) DO UPDATE SET
			start_date = excluded.start_date,
			end_date = excluded.end_date,
			title = excluded.title,
			focus_md = excluded.focus_md,
			signals_md = excluded.signals_md,
			trends_md = excluded.trends_md,
			takeaways_md = excluded.takeaways_md,
			ponder_md = excluded.ponder_md,
			full_md = excluded.full_md,
			daily_issue_ids = excluded.daily_issue_ids,
			status = excluded.status,
			generated_at = excluded.generated_at,
			published_at = excluded.published_at
		RETURNING id
	`
	status := w.Status
	if status == "" {
		status = IssueStatusDraft
	}
	var id int64
	err := s.db.QueryRowContext(ctx, q,
		w.DomainID, w.Year, w.Week,
		w.StartDate.Format("2006-01-02"), w.EndDate.Format("2006-01-02"),
		w.Title,
		w.FocusMD, w.SignalsMD, w.TrendsMD, w.TakeawaysMD, w.PonderMD,
		w.FullMD, w.DailyIssueIDs, status,
		nullTimePtr(w.GeneratedAt), nullTimePtr(w.PublishedAt),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("upsert weekly %s/%d-W%02d: %w", w.DomainID, w.Year, w.Week, err)
	}
	return id, nil
}

func (s *sqliteStore) GetWeeklyIssue(ctx context.Context, domainID string, year, week int) (*WeeklyIssue, error) {
	const q = `
		SELECT id, domain_id, year, week, start_date, end_date,
		       COALESCE(title, ''), COALESCE(focus_md, ''),
		       COALESCE(signals_md, ''), COALESCE(trends_md, ''),
		       COALESCE(takeaways_md, ''), COALESCE(ponder_md, ''),
		       COALESCE(full_md, ''), COALESCE(daily_issue_ids, ''),
		       status, generated_at, published_at
		FROM weekly_issues
		WHERE domain_id = ? AND year = ? AND week = ?
	`
	var w WeeklyIssue
	var generatedAt, publishedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, q, domainID, year, week).Scan(
		&w.ID, &w.DomainID, &w.Year, &w.Week, &w.StartDate, &w.EndDate,
		&w.Title, &w.FocusMD, &w.SignalsMD, &w.TrendsMD,
		&w.TakeawaysMD, &w.PonderMD, &w.FullMD, &w.DailyIssueIDs,
		&w.Status, &generatedAt, &publishedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("get weekly %s/%d-W%02d: %w", domainID, year, week, err)
	}
	if generatedAt.Valid {
		t := generatedAt.Time
		w.GeneratedAt = &t
	}
	if publishedAt.Valid {
		t := publishedAt.Time
		w.PublishedAt = &t
	}
	return &w, nil
}

func (s *sqliteStore) ListDailyIssuesByDateRange(ctx context.Context, domainID string, start, end time.Time) ([]*Issue, error) {
	const q = `
		SELECT id, domain_id, issue_date, COALESCE(issue_number, 0),
		       COALESCE(title, ''), COALESCE(summary, ''), status,
		       COALESCE(source_count, 0), COALESCE(item_count, 0),
		       generated_at, published_at
		FROM issues
		WHERE domain_id = ? AND issue_date >= ? AND issue_date <= ?
		ORDER BY issue_date ASC
	`
	rows, err := s.db.QueryContext(ctx, q, domainID,
		start.Format("2006-01-02"), end.Format("2006-01-02"))
	if err != nil {
		return nil, fmt.Errorf("list issues %s [%s..%s]: %w",
			domainID, start.Format("2006-01-02"), end.Format("2006-01-02"), err)
	}
	defer rows.Close()

	var out []*Issue
	for rows.Next() {
		var is Issue
		var generatedAt, publishedAt sql.NullTime
		if err := rows.Scan(
			&is.ID, &is.DomainID, &is.IssueDate, &is.IssueNumber,
			&is.Title, &is.Summary, &is.Status,
			&is.SourceCount, &is.ItemCount,
			&generatedAt, &publishedAt,
		); err != nil {
			return nil, fmt.Errorf("scan issue: %w", err)
		}
		if generatedAt.Valid {
			t := generatedAt.Time
			is.GeneratedAt = &t
		}
		if publishedAt.Valid {
			t := publishedAt.Time
			is.PublishedAt = &t
		}
		out = append(out, &is)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate issues: %w", err)
	}
	return out, nil
}

// -------- Delivery --------

func (s *sqliteStore) InsertDelivery(ctx context.Context, d *Delivery) error {
	const q = `
		INSERT INTO deliveries (issue_id, channel, target, status, response_json, sent_at)
		VALUES (?, ?, ?, ?, ?, COALESCE(?, CURRENT_TIMESTAMP))
	`
	var sentAt any
	if !d.SentAt.IsZero() {
		sentAt = d.SentAt
	}
	if _, err := s.db.ExecContext(ctx, q,
		d.IssueID, d.Channel, d.Target, d.Status, d.ResponseJSON, sentAt,
	); err != nil {
		return fmt.Errorf("insert delivery (issue=%d channel=%s): %w", d.IssueID, d.Channel, err)
	}
	return nil
}

func (s *sqliteStore) ListDeliveries(ctx context.Context, issueID int64) ([]*Delivery, error) {
	const q = `
		SELECT id, issue_id, channel, COALESCE(target, ''), status,
		       COALESCE(response_json, ''), sent_at
		FROM deliveries
		WHERE issue_id = ?
		ORDER BY sent_at, id
	`
	rows, err := s.db.QueryContext(ctx, q, issueID)
	if err != nil {
		return nil, fmt.Errorf("list deliveries %d: %w", issueID, err)
	}
	defer rows.Close()

	var out []*Delivery
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(
			&d.ID, &d.IssueID, &d.Channel, &d.Target, &d.Status, &d.ResponseJSON, &d.SentAt,
		); err != nil {
			return nil, fmt.Errorf("scan delivery: %w", err)
		}
		out = append(out, &d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate deliveries: %w", err)
	}
	return out, nil
}

// -------- helpers --------

func nullString(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func nullInt(n int64) any {
	if n == 0 {
		return nil
	}
	return n
}

func nullTimePtr(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return *t
}
