// Package main is the briefing-v3 CLI entry point.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"briefing-v3/internal/config"
	"briefing-v3/internal/store"
)

const usage = `briefing-v3 — AI daily briefing generator

Usage:
    briefing <command> [flags]

Commands:
    migrate     Initialize or migrate the SQLite schema
    seed        Load sources from config/ai.yaml into the database
    run         Fetch + classify + compose + render + publish (main pipeline)
    regen       Reuse existing SQLite data, rebuild infocard + HTML + push
    serve       Start static file server for docs/ (web viewer)
    promote     Manually promote an existing issue to Slack prod channel
    status      Show the status of a specific issue
    help        Show this help message

Flags (available on most commands):
    -c, --config string   YAML config path (default "config/ai.yaml")
    -d, --date string     Issue date YYYY-MM-DD (default today in Asia/Shanghai)
        --domain string   Domain id (default "ai")
        --target string   test|auto|prod (default "test")
        --dry-run         Skip actual Slack push
        --no-images       Skip mediaextract / infocard / headline PNG stages
                          (text-only mode, used as an escape hatch when image
                          pipeline is unstable)

Serve flags:
        --port int        listen port (default 8080)
        --docs string     directory to serve (default "docs")
        --addr string     bind address (default "0.0.0.0")
`

// globalFlags are shared by most subcommands. They are parsed once at
// entry and passed down to each command implementation.
type globalFlags struct {
	configPath string
	dateStr    string
	domain     string
	target     string
	dryRun     bool
	noImages   bool
	mediaOnly  bool
}

// parseGlobalFlags parses the flag set used by every command. Unknown
// flags cause FlagSet.ExitOnError to terminate with a usage message.
func parseGlobalFlags(args []string) (*globalFlags, []string) {
	fs := flag.NewFlagSet("briefing", flag.ExitOnError)
	gf := &globalFlags{}
	fs.StringVar(&gf.configPath, "config", "config/ai.yaml", "YAML config path")
	fs.StringVar(&gf.configPath, "c", "config/ai.yaml", "YAML config path (shorthand)")
	fs.StringVar(&gf.dateStr, "date", "", "issue date YYYY-MM-DD")
	fs.StringVar(&gf.dateStr, "d", "", "issue date (shorthand)")
	fs.StringVar(&gf.domain, "domain", "ai", "domain id")
	fs.StringVar(&gf.target, "target", "test", "test|auto|prod")
	fs.BoolVar(&gf.dryRun, "dry-run", false, "skip actual slack push")
	fs.BoolVar(&gf.noImages, "no-images", false, "skip all image generation stages (mediaextract/infocard/headline)")
	fs.BoolVar(&gf.mediaOnly, "media-only", false, "regen only: skip infocard template PNGs, use mediaextract og:image instead (hubtoday style)")
	_ = fs.Parse(args)
	return gf, fs.Args()
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd := os.Args[1]
	if cmd == "help" || cmd == "-h" || cmd == "--help" {
		fmt.Print(usage)
		return
	}

	// `serve` has its own flag set, handle it before parseGlobalFlags
	// (which understands --date, --target etc that are irrelevant here).
	if cmd == "serve" {
		if err := serveCommand(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "serve error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	gf, _ := parseGlobalFlags(os.Args[2:])

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cfg, err := config.Load(gf.configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "config error: %v\n", err)
		os.Exit(1)
	}

	date, err := resolveDate(gf.dateStr, cfg.Domain.Timezone)
	if err != nil {
		fmt.Fprintf(os.Stderr, "date error: %v\n", err)
		os.Exit(1)
	}

	switch cmd {
	case "migrate":
		err = migrateCommand(ctx, cfg)
	case "seed":
		err = seedCommand(ctx, cfg)
	case "run":
		err = runCommand(ctx, cfg, date, gf)
	case "regen":
		err = regenCommand(ctx, cfg, date, gf)
	case "promote":
		err = promoteCommand(ctx, cfg, date, gf)
	case "status":
		err = statusCommand(ctx, cfg, date, gf)
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", cmd)
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// resolveDate parses a YYYY-MM-DD string in the given timezone, or
// returns today's midnight in that timezone when s is empty.
func resolveDate(s, tz string) (time.Time, error) {
	loc, err := time.LoadLocation(tz)
	if err != nil || loc == nil {
		loc = time.FixedZone("CST", 8*3600)
	}
	if s == "" {
		now := time.Now().In(loc)
		return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, loc), nil
	}
	t, err := time.ParseInLocation("2006-01-02", s, loc)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date %q: %w", s, err)
	}
	return t, nil
}

// migrateCommand initializes the SQLite schema.
func migrateCommand(ctx context.Context, cfg *config.Config) error {
	s, err := store.New("data/briefing.db")
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		return err
	}
	fmt.Println("migrate: OK")
	return nil
}

// seedCommand inserts the domain + all enabled sources from config into DB.
func seedCommand(ctx context.Context, cfg *config.Config) error {
	s, err := store.New("data/briefing.db")
	if err != nil {
		return err
	}
	defer s.Close()

	// Ensure schema exists before inserting.
	if err := s.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	// Upsert domain record.
	if err := s.UpsertDomain(ctx, &store.Domain{
		ID:         cfg.Domain.ID,
		Name:       cfg.Domain.Name,
		ConfigPath: "config/ai.yaml",
	}); err != nil {
		return fmt.Errorf("upsert domain: %w", err)
	}

	// Upsert each enabled source. We serialize the full SourceConfig so
	// adapters can recover type-specific options (query/hl/gl/limit/...).
	inserted := 0
	for _, src := range cfg.EnabledSources() {
		cfgJSON, err := marshalSourceConfig(src)
		if err != nil {
			return fmt.Errorf("marshal source %s: %w", src.ID, err)
		}
		_, err = s.UpsertSource(ctx, &store.Source{
			DomainID:   cfg.Domain.ID,
			Type:       src.Type,
			Name:       src.Name,
			ConfigJSON: cfgJSON,
			Enabled:    src.Enabled,
		})
		if err != nil {
			return fmt.Errorf("upsert source %s: %w", src.ID, err)
		}
		inserted++
	}
	fmt.Printf("seed: %d sources upserted\n", inserted)
	return nil
}

// marshalSourceConfig serializes a SourceConfig to a JSON string blob
// suitable for storing in sources.config_json.
func marshalSourceConfig(src config.SourceConfig) (string, error) {
	// Build a flat map combining the explicit fields with the inline Extra,
	// so adapters can unmarshal into whatever shape they prefer.
	payload := map[string]any{
		"id":       src.ID,
		"type":     src.Type,
		"category": src.Category,
		"name":     src.Name,
		"url":      src.URL,
		"enabled":  src.Enabled,
		"priority": src.Priority,
	}
	for k, v := range src.Extra {
		payload[k] = v
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// runCommand is the main pipeline entry point. The actual wiring of every
// stage lives in run.go so this file stays focused on CLI plumbing.
func runCommand(ctx context.Context, cfg *config.Config, date time.Time, gf *globalFlags) error {
	return runPipeline(ctx, cfg, date, gf)
}

// payloadSnapshotPath returns the canonical file path that run/regen
// use to persist the last Slack payload they POSTed to the test
// channel. promoteCommand reads this file and re-POSTs the EXACT same
// bytes to the prod webhook so "manual sign-off then promote" cannot
// ever drift from what the reviewer saw in test.
func payloadSnapshotPath(date time.Time) string {
	return fmt.Sprintf("data/slack-payload-%s.json", date.Format("2006-01-02"))
}

// savePayloadSnapshot writes payload verbatim to payloadSnapshotPath(date).
// Used by run and regen right before they POST to test, so the prod
// promote path can re-send bytes that are guaranteed to match exactly.
func savePayloadSnapshot(date time.Time, payload []byte) error {
	path := payloadSnapshotPath(date)
	if err := os.MkdirAll("data", 0o755); err != nil {
		return fmt.Errorf("mkdir data: %w", err)
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// promoteCommand re-posts the SAME Slack payload that was saved during
// the last run/regen to the prod webhook. The user workflow is:
//
//  1. `briefing run --target test` (or `regen --media-only --target test`)
//     — generates + POSTs to test webhook + persists payload snapshot
//  2. Reviewer inspects the test channel, confirms the content is good
//  3. `briefing promote --date YYYY-MM-DD` — reads the snapshot and
//     POSTs the EXACT same bytes to the prod webhook
//
// This guarantees test and prod see identical blocks, even though the
// underlying pipeline (LLM, mediaextract, etc.) is non-deterministic
// between invocations.
func promoteCommand(ctx context.Context, cfg *config.Config, date time.Time, gf *globalFlags) error {
	path := payloadSnapshotPath(date)
	payload, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read snapshot %s: %w (did you run `briefing run` / `briefing regen` first?)", path, err)
	}
	if len(payload) == 0 {
		return fmt.Errorf("snapshot %s is empty", path)
	}
	prodURL := cfg.Slack.ProdWebhook
	if prodURL == "" {
		return errors.New("cfg.Slack.ProdWebhook is empty — set SLACK_PROD_WEBHOOK in secrets.env")
	}

	fmt.Printf("[%s] promote: re-posting snapshot %s (%d bytes) to Slack prod webhook\n",
		time.Now().Format("15:04:05"), path, len(payload))

	if gf.dryRun {
		fmt.Println("dry-run: not posting to prod")
		fmt.Println(string(payload))
		return nil
	}

	s, err := store.New("data/briefing.db")
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()

	issue, err := s.GetIssueByDate(ctx, gf.domain, date)
	if err != nil || issue == nil {
		// The issue row is only used to record a Delivery log entry.
		// If it is missing we still want to be able to promote, so we
		// fall back to issueID 0 — the Slack POST itself does not need
		// it.
		fmt.Printf("[WARN] get issue for delivery log: %v (proceeding with issueID=0)\n", err)
	}
	var issueID int64
	if issue != nil {
		issueID = issue.ID
	}

	delivery := postSlackPayload(ctx, store.ChannelSlackProd, prodURL, payload, issueID)
	if s != nil && issue != nil {
		if err := s.InsertDelivery(ctx, delivery); err != nil {
			fmt.Printf("[WARN] insert prod delivery: %v\n", err)
		}
	}
	if delivery.Status != store.DeliveryStatusSent {
		return fmt.Errorf("slack prod publish failed: %s", delivery.ResponseJSON)
	}
	fmt.Printf("[%s] promote: slack prod OK\n", time.Now().Format("15:04:05"))
	return nil
}

// statusCommand prints the current issue + deliveries for the given date.
// Filled in by the main thread once the pipeline is wired.
func statusCommand(ctx context.Context, cfg *config.Config, date time.Time, gf *globalFlags) error {
	fmt.Printf("status: date=%s (not yet implemented)\n", date.Format("2006-01-02"))
	return nil
}
