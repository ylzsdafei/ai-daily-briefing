// cmd/briefing/run.go — the real `briefing run` implementation.
//
// This file wires together every package that Wave 1 + Wave 2 produced:
//
//	store → ingest (concurrent) → rank → classify → compose → generate
//	      → gate → render (markdown + Slack payload) → image (headline PNG)
//	      → publish (Slack webhook, test + optional prod)
//
// It is the ONLY place where all pipeline stages are aware of each other.
// Individual packages stay single-purpose and loosely coupled.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"briefing-v3/internal/classify"
	"briefing-v3/internal/compose"
	"briefing-v3/internal/config"
	"briefing-v3/internal/gate"
	"briefing-v3/internal/generate"
	"briefing-v3/internal/image"
	"briefing-v3/internal/infocard"
	"briefing-v3/internal/ingest"
	"briefing-v3/internal/mediaextract"
	"briefing-v3/internal/publish"
	"briefing-v3/internal/rank"
	"briefing-v3/internal/render"
	"briefing-v3/internal/store"
)

// runPipeline executes the full briefing-v3 flow for a single date/domain.
// It is called by runCommand in main.go. Every stage prints a progress line
// to stdout so that operators watching a dry-run can see where time is
// being spent.
//
// The function NEVER silently degrades: if any mandatory stage fails it
// returns a non-nil error which the caller propagates as process exit 1
// (and scripts/cron.sh will then post an alert to the Slack test channel).
func runPipeline(ctx context.Context, cfg *config.Config, date time.Time, gf *globalFlags) error {
	stage := func(name string) { fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), name) }

	stage(fmt.Sprintf("pipeline start: date=%s domain=%s target=%s dryRun=%v",
		date.Format("2006-01-02"), gf.domain, gf.target, gf.dryRun))

	// --- 0. Open store & ensure schema ----------------------------------
	s, err := store.New("data/briefing.db")
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	// --- 1. Upsert the Issue row for today ------------------------------
	issue := &store.Issue{
		DomainID:  gf.domain,
		IssueDate: date,
		Title:     fmt.Sprintf("AI资讯日报 %d/%d/%d", date.Year(), int(date.Month()), date.Day()),
		Status:    store.IssueStatusDraft,
	}
	issueID, err := s.UpsertIssue(ctx, issue)
	if err != nil {
		return fmt.Errorf("upsert issue: %w", err)
	}
	issue.ID = issueID
	stage(fmt.Sprintf("issue ready: id=%d", issueID))

	// --- 2. Concurrent ingest -------------------------------------------
	stage("ingest: starting concurrent fetch")
	rawItems, ingestStats, err := ingestAll(ctx, s, gf.domain, 20*time.Second)
	if err != nil {
		return fmt.Errorf("ingest: %w", err)
	}
	stage(fmt.Sprintf("ingest: collected %d raw items across %d sources (%d ok, %d failed)",
		len(rawItems), ingestStats.total, ingestStats.ok, ingestStats.failed))
	if len(rawItems) == 0 {
		return errors.New("ingest: zero raw items collected — cannot proceed")
	}

	// --- 3. Persist raw items (idempotent ON CONFLICT) ------------------
	if err := s.InsertRawItems(ctx, rawItems); err != nil {
		return fmt.Errorf("insert raw items: %w", err)
	}
	stage(fmt.Sprintf("store: %d raw items persisted", len(rawItems)))

	// Assign an in-memory sequential ID to every raw item so that the
	// downstream rank.Rank() can build its byID map without collisions.
	// The store layer does not back-fill AUTOINCREMENT ids on bulk insert,
	// so rawItems[].ID would otherwise all stay 0 — we saw this in the
	// first dry-run where rank collapsed 967 items into 1. The temporary
	// ID is only used for LLM batching and is not persisted; compose and
	// render never cross back to raw_items via this id.
	for i, it := range rawItems {
		if it != nil {
			it.ID = int64(i + 1)
		}
	}

	// --- 4. Filter by time window ---------------------------------------
	cutoff := date.Add(-time.Duration(cfg.Window.LookbackHours) * time.Hour)
	filtered := filterByWindow(rawItems, cutoff)
	stage(fmt.Sprintf("filter: %d → %d items within %dh", len(rawItems), len(filtered), cfg.Window.LookbackHours))

	// If not enough in the strict window, relax to extended window.
	if len(filtered) < cfg.Gate.MinItems && cfg.Window.ExtendedHours > cfg.Window.LookbackHours {
		cutoff2 := date.Add(-time.Duration(cfg.Window.ExtendedHours) * time.Hour)
		filtered = filterByWindow(rawItems, cutoff2)
		stage(fmt.Sprintf("filter: extended window to %dh → %d items", cfg.Window.ExtendedHours, len(filtered)))
	}

	if len(filtered) == 0 {
		return errors.New("filter: zero items inside lookback window — cannot proceed")
	}

	// --- 5. Rank (LLM quality scoring) ----------------------------------
	stage("rank: calling LLM quality scorer")
	ranker, err := rank.New(rank.Config{
		BaseURL: cfg.LLM.BaseURL,
		APIKey:  cfg.LLM.APIKey,
		Model:   cfg.LLM.Model,
		Timeout: cfg.LLM.LLMTimeout(),
	})
	if err != nil {
		return fmt.Errorf("rank new: %w", err)
	}
	ranked, err := ranker.Rank(ctx, filtered)
	if err != nil {
		return fmt.Errorf("rank: %w", err)
	}
	stage(fmt.Sprintf("rank: %d → %d high-quality items", len(filtered), len(ranked)))
	if len(ranked) == 0 {
		return errors.New("rank: LLM returned zero items above quality threshold")
	}

	// Extract just the RawItem from each RankedItem, preserving rank order.
	rankedRaws := make([]*store.RawItem, 0, len(ranked))
	for _, r := range ranked {
		if r.Item != nil {
			rankedRaws = append(rankedRaws, r.Item)
		}
	}

	// --- 6. Classify (LLM section assignment) ---------------------------
	stage("classify: calling LLM section classifier")
	classifier, err := classify.New(classify.Config{
		BaseURL: cfg.LLM.BaseURL,
		APIKey:  cfg.LLM.APIKey,
		Model:   cfg.LLM.Model,
		Timeout: cfg.LLM.LLMTimeout(),
	})
	if err != nil {
		return fmt.Errorf("classify new: %w", err)
	}
	sectioned, err := classifier.Classify(ctx, rankedRaws)
	if err != nil {
		return fmt.Errorf("classify: %w", err)
	}
	for secID, secItems := range sectioned {
		stage(fmt.Sprintf("classify: %s → %d items", secID, len(secItems)))
	}

	// --- 7. Compose (LLM Step 1B text generation per section) ----------
	stage("compose: calling LLM summarizer per section")
	generator, err := generate.New(generate.Config{
		BaseURL:     cfg.LLM.BaseURL,
		APIKey:      cfg.LLM.APIKey,
		Model:       cfg.LLM.Model,
		Temperature: cfg.LLM.Temperature,
		MaxTokens:   cfg.LLM.MaxTokens,
		Timeout:     cfg.LLM.LLMTimeout(),
		MaxRetries:  cfg.LLM.MaxRetries,
	})
	if err != nil {
		return fmt.Errorf("generate new: %w", err)
	}
	summarizer, ok := generator.(generate.Summarizer)
	if !ok {
		return errors.New("generate: openai generator does not implement Summarizer")
	}

	composer := compose.New()
	composeSections := make([]compose.SectionConfig, 0, len(cfg.Sections))
	for _, sec := range cfg.Sections {
		composeSections = append(composeSections, compose.SectionConfig{
			ID:       sec.ID,
			Title:    sec.Title,
			MinItems: sec.MinItems,
			MaxItems: sec.MaxItems,
		})
	}
	issueItems, err := composer.Compose(ctx, issueID, sectioned, composeSections, summarizer)
	if err != nil {
		return fmt.Errorf("compose: %w", err)
	}
	stage(fmt.Sprintf("compose: produced %d issue items", len(issueItems)))

	// --- 7b. Extract hero image/video from source URLs (fallback only) ----
	// This is the fallback media path. The primary path is infocard
	// (editorial-style PIL info cards) built below from LLM-distilled
	// structured JSON. mediaextract only runs to give items a hotlink
	// image/video IF the info-card generation later fails.
	stage("media: extracting fallback hero image/video from source URLs")
	mediaFound := enrichItemsWithMedia(ctx, issueItems)
	stage(fmt.Sprintf("media: %d items got a fallback hero image/video", mediaFound))

	// --- 8. Persist IssueItems (replace any existing for this issue) ----
	if err := s.ReplaceIssueItems(ctx, issueID, issueItems); err != nil {
		return fmt.Errorf("replace issue items: %w", err)
	}

	// --- 9. Generate insight (Step 3 — industry + takeaways) ----------
	stage("insight: calling LLM for industry + takeaways")
	insight, err := generator.GenerateInsight(ctx, &generate.Input{
		Issue:    issue,
		Items:    issueItems,
		RawItems: rankedRaws,
	})
	if err != nil {
		return fmt.Errorf("generate insight: %w", err)
	}
	insight.IssueID = issueID
	if err := s.UpsertIssueInsight(ctx, insight); err != nil {
		return fmt.Errorf("upsert insight: %w", err)
	}
	stage("insight: generated and persisted")

	// --- 10. Daily summary (Step 2 — 3-line summary) --------------------
	stage("summary: generating 3-line daily summary")
	summary, err := generateDailySummary(ctx, cfg.LLM, issueItems)
	if err != nil {
		// Summary failure is a hard stop — upstream always has a summary.
		return fmt.Errorf("generate summary: %w", err)
	}
	issue.Summary = summary
	issue.ItemCount = len(issueItems)
	issue.SourceCount = countSourceDomains(issueItems)
	now := time.Now()
	issue.GeneratedAt = &now
	issue.Status = store.IssueStatusGenerated
	if _, err := s.UpsertIssue(ctx, issue); err != nil {
		return fmt.Errorf("update issue after generate: %w", err)
	}

	// --- 10b. Info cards (primary visual) ------------------------------
	// One LLM call distills ALL items + the whole-issue header into
	// structured JSON; then we shell out to PIL for the editorial-style
	// PNGs (1 header + N item cards). Each card PNG is injected as a
	// markdown image at the top of its IssueItem.BodyMD so the HTML
	// renderer picks it up via the existing `![alt](url)` path.
	stage("infocard: generating editorial info-card JSON via LLM")
	icGen, icErr := infocard.New(infocard.Config{
		BaseURL:    cfg.LLM.BaseURL,
		APIKey:     cfg.LLM.APIKey,
		Model:      cfg.LLM.Model,
		MaxRetries: 3,
		Timeout:    cfg.LLM.LLMTimeout(),
	})
	var headerCardPNGRel string
	if icErr != nil {
		fmt.Printf("[WARN] infocard new: %v — falling back to mediaextract images only\n", icErr)
	} else {
		// compose.Seq restarts per section (1..N), so multiple items across
		// different sections can share Seq=1,2,3… Passing those to the LLM
		// would collapse all cards with the same seq onto the same PNG
		// filename. Build a UID-remapped shadow slice where every item has
		// a globally-unique Seq (1..totalItems), pass the shadows to the
		// infocard LLM, then match the returned cards back via UID.
		shadowItems := make([]*store.IssueItem, 0, len(issueItems))
		uidToItem := make(map[int]*store.IssueItem, len(issueItems))
		for i, it := range issueItems {
			if it == nil {
				continue
			}
			shadow := *it
			shadow.Seq = i + 1
			shadowItems = append(shadowItems, &shadow)
			uidToItem[shadow.Seq] = it
		}

		header, cards, err := icGen.Generate(ctx, shadowItems, summary)
		if err != nil {
			fmt.Printf("[WARN] infocard generate: %v — falling back to mediaextract images only\n", err)
		} else {
			stage(fmt.Sprintf("infocard: got header + %d cards, rendering PNGs", len(cards)))
			header.IssueDate = date.Format("2006-01-02")
			cardDir := filepath.Join("data", "images", "cards", date.Format("2006-01-02"))

			// Render header PNG (whole-issue 大字报). A failure here is
			// non-fatal — we continue to render item cards.
			headerPath := filepath.Join(cardDir, "header.png")
			if err := renderInfoCardPNG(ctx, "header", header, headerPath); err != nil {
				fmt.Printf("[WARN] infocard header render: %v\n", err)
			} else {
				headerCardPNGRel = fmt.Sprintf("../data/images/cards/%s/header.png", date.Format("2006-01-02"))
				stage(fmt.Sprintf("infocard: header PNG written to %s", headerPath))
			}

			// Render per-item cards and inject markdown image at top.
			// Every individual card failure is isolated with recover() +
			// continue so one broken item can never take down the run.
			renderedCount := 0
			for _, c := range cards {
				if c == nil {
					continue
				}
				it := uidToItem[c.ItemSeq]
				if it == nil {
					fmt.Printf("[WARN] infocard: card uid=%d has no matching item, skip\n", c.ItemSeq)
					continue
				}
				func() {
					defer func() {
						if r := recover(); r != nil {
							fmt.Printf("[WARN] infocard uid=%d panic: %v\n", c.ItemSeq, r)
						}
					}()
					outPath := filepath.Join(cardDir, fmt.Sprintf("item-%d.png", c.ItemSeq))
					if err := renderInfoCardPNG(ctx, "item", c, outPath); err != nil {
						fmt.Printf("[WARN] infocard item uid=%d render: %v\n", c.ItemSeq, err)
						return
					}
					renderedCount++
					relPath := fmt.Sprintf("../data/images/cards/%s/item-%d.png", date.Format("2006-01-02"), c.ItemSeq)
					alt := strings.TrimSpace(c.MainTitle)
					if alt == "" {
						alt = strings.TrimSpace(it.Title)
					}
					for _, ch := range []string{"[", "]", "(", ")"} {
						alt = strings.ReplaceAll(alt, ch, " ")
					}
					alt = strings.TrimSpace(alt)
					imgLine := fmt.Sprintf("![%s](%s)\n\n", alt, relPath)
					it.BodyMD = imgLine + strings.TrimLeft(it.BodyMD, "\n")
				}()
			}
			stage(fmt.Sprintf("infocard: rendered %d/%d item PNGs", renderedCount, len(cards)))

			// Persist the mutated items (now with image markdown at top).
			// A store failure here is non-fatal — HTML is re-rendered from
			// the in-memory slice below anyway.
			if err := s.ReplaceIssueItems(ctx, issueID, issueItems); err != nil {
				fmt.Printf("[WARN] replace issue items after infocard: %v\n", err)
			}
		}
	}

	// --- 11. Render markdown + sections map ----------------------------
	renderSecs := make([]render.SectionMeta, 0, len(cfg.Sections))
	for _, sec := range cfg.Sections {
		renderSecs = append(renderSecs, render.SectionMeta{
			ID:    sec.ID,
			Title: sec.Title,
		})
	}
	fullMarkdown := render.RenderMarkdown(issue, issueItems, insight, renderSecs)
	sectionsMD := render.RenderSectionsMap(issueItems, renderSecs)
	stage(fmt.Sprintf("render: markdown built (%d bytes)", len(fullMarkdown)))

	// Also persist the full markdown to daily/YYYY-MM-DD.md so git history
	// and manual review always have a flat text copy.
	_ = writeDailyMarkdown(date, fullMarkdown)

	// --- 12. Generate headline image (local PNG only; Slack image_url
	//         stays empty until we have a public image host) ------------
	var headlineImageURL string
	headlineText := extractTopHeadline(issueItems, summary)
	if cfg.Image.Enabled {
		stage(fmt.Sprintf("image: generating headline PNG — %q", headlineText))
		imgRenderer := image.New(image.Config{
			PythonBin:   cfg.Image.PythonBin,
			ScriptPath:  cfg.Image.GeneratorScript,
			OutputDir:   cfg.Image.OutputDir,
			Width:       cfg.Image.Width,
			Height:      cfg.Image.Height,
			FontBold:    cfg.Image.FontBold,
			FontRegular: cfg.Image.FontRegular,
			Timeout:     30 * time.Second,
		})
		subtitle := fmt.Sprintf("briefing-v3 · %s", date.Format("2006-01-02"))
		pngPath, imgErr := imgRenderer.Render(ctx, date, headlineText, subtitle)
		if imgErr != nil {
			// Image failure is NOT a hard stop in v1.0.0 — Slack still gets
			// the text payload. Log the error prominently so the operator
			// knows the cover is missing.
			fmt.Printf("[WARN] image render failed: %v\n", imgErr)
		} else {
			stage(fmt.Sprintf("image: PNG ready at %s", pngPath))
			// v1.0.0 does NOT have a public image host yet. Keep
			// headlineImageURL empty so Slack render.BuildSlackPayload
			// gracefully skips the image block. The PNG is still on
			// disk as evidence and v1.0.1 will wire a git-raw CDN.
		}
	}

	// --- 12b. Write HTML page + refresh index.html ---------------------
	// Prefer the editorial info-card header (大字报) as the hero image.
	// Fall back to the old gen_headline.py PNG only if the info-card
	// pass did not produce a header file.
	headlineRelForHTML := headerCardPNGRel
	if headlineRelForHTML == "" && cfg.Image.Enabled {
		// The PNG lives at data/images/YYYY-MM-DD.png; docs/*.html sits
		// one level deep under briefing-v3/, so the relative href is
		// ../data/images/... which browsers open correctly via file://.
		headlineRelForHTML = fmt.Sprintf("../data/images/%s.png", date.Format("2006-01-02"))
	}
	htmlRes, htmlErr := render.WriteIssueHTML("docs", &render.IssueHTMLInput{
		Issue:       issue,
		Items:       issueItems,
		Insight:     insight,
		Sections:    renderSecs,
		HeadlineImg: headlineRelForHTML,
	})
	if htmlErr != nil {
		fmt.Printf("[WARN] html page generation failed: %v\n", htmlErr)
	} else {
		stage(fmt.Sprintf("html: %s (%d bytes)", htmlRes.Path, htmlRes.Size))
	}
	if indexEntries, err := render.CollectIndexEntries("docs"); err == nil {
		if _, err := render.WriteIndexHTML("docs", indexEntries, "briefing-v3 · 每日早读 · 全网深度聚合"); err != nil {
			fmt.Printf("[WARN] index html refresh failed: %v\n", err)
		}
	}

	// --- 13. Build RenderedIssue for downstream render/publish ---------
	// ReportURL points at the local HTML page via an absolute file:// URI
	// so that Slack buttons at least identify the right file during
	// development. Once GitHub Pages (or another host) is configured, set
	// an env var BRIEFING_REPORT_URL_BASE to override this with a public
	// URL. Example: https://ylzsdafei.github.io/briefing-v3/{{DATE}}.html
	reportURL := fmt.Sprintf("file:///root/briefing-v3/docs/%s.html", date.Format("2006-01-02"))
	if base := os.Getenv("BRIEFING_REPORT_URL_BASE"); base != "" {
		reportURL = strings.Replace(base, "{{DATE}}", date.Format("2006-01-02"), 1)
	}

	rendered := &publish.RenderedIssue{
		Issue:            issue,
		Items:            issueItems,
		Insight:          insight,
		HeadlineImageURL: headlineImageURL,
		SectionsMarkdown: sectionsMD,
		DateZH:           render.FormatDateZH(issue),
		ReportURL:        reportURL,
	}

	// --- 14. Hard quality gate -----------------------------------------
	stage("gate: checking hard quality rules")
	g := gate.New(gate.Config{
		MinItems:               cfg.Gate.MinItems,
		MinSectionsWithContent: cfg.Gate.MinSectionsWithContent,
		MinInsightChars:        cfg.Gate.MinInsightChars,
		MinIndustryBullets:     cfg.Gate.MinIndustryBullets,
		MaxIndustryBullets:     cfg.Gate.MaxIndustryBullets,
		MinTakeawayBullets:     cfg.Gate.MinTakeawayBullets,
		MaxTakeawayBullets:     cfg.Gate.MaxTakeawayBullets,
		MinSourceDomains:       cfg.Gate.MinSourceDomains,
	})
	report := g.Check(issue, issueItems, insight)
	stage(fmt.Sprintf("gate: pass=%v items=%d sections=%d insightChars=%d industry=%d takeaways=%d domains=%d",
		report.Pass, report.ItemCount, report.SectionCount, report.InsightChars,
		report.IndustryBullets, report.TakeawayBullets, report.SourceDomainCount))
	if !report.Pass {
		for _, reason := range report.Reasons {
			fmt.Printf("[GATE FAIL] %s\n", reason)
		}
	}

	// --- 15. Build the Slack payload once (shared between test + prod) -
	slackPayload, err := render.BuildSlackPayload(rendered)
	if err != nil {
		return fmt.Errorf("render slack payload: %w", err)
	}

	// Dry-run short-circuit: print the markdown + payload to stdout and stop.
	if gf.dryRun {
		stage("dry-run: skipping actual publish")
		fmt.Println("\n================ FULL MARKDOWN ================")
		fmt.Println(fullMarkdown)
		fmt.Println("================ SLACK PAYLOAD ================")
		fmt.Println(string(slackPayload))
		fmt.Println("===============================================")
		return nil
	}

	// --- 16. Publish to Slack test (unconditional) ---------------------
	stage("publish: posting to Slack test channel")
	testDelivery := postSlackPayload(ctx, store.ChannelSlackTest, cfg.Slack.TestWebhook, slackPayload, issueID)
	if err := s.InsertDelivery(ctx, testDelivery); err != nil {
		fmt.Printf("[WARN] insert test delivery: %v\n", err)
	}
	if testDelivery.Status != store.DeliveryStatusSent {
		return fmt.Errorf("slack test publish failed: %s", testDelivery.ResponseJSON)
	}
	stage("publish: slack test OK")

	// --- 17. Publish to Slack prod if gate passed & target == auto|prod -
	targetWantsProd := gf.target == "auto" || gf.target == "prod"
	if targetWantsProd {
		if !report.Pass {
			// Gate failed but target wants prod: post alert to test, do not
			// touch prod. This is the "不允许失败" safety rail.
			alertMsg := buildGateFailAlert(issue, report)
			alertBody, _ := json.Marshal(map[string]any{"text": alertMsg})
			_ = postAlert(ctx, cfg.Slack.TestWebhook, alertBody)
			return fmt.Errorf("gate failed (%d reasons), prod channel skipped: %s",
				len(report.Reasons), strings.Join(report.Reasons, "; "))
		}
		stage("publish: gate passed → posting to Slack prod channel")
		prodDelivery := postSlackPayload(ctx, store.ChannelSlackProd, cfg.Slack.ProdWebhook, slackPayload, issueID)
		if err := s.InsertDelivery(ctx, prodDelivery); err != nil {
			fmt.Printf("[WARN] insert prod delivery: %v\n", err)
		}
		if prodDelivery.Status != store.DeliveryStatusSent {
			return fmt.Errorf("slack prod publish failed: %s", prodDelivery.ResponseJSON)
		}
		stage("publish: slack prod OK")
	} else {
		stage("publish: target=test, skipping prod channel")
	}

	// --- 18. Mark issue as published -----------------------------------
	if err := s.MarkIssuePublished(ctx, issueID); err != nil {
		return fmt.Errorf("mark published: %w", err)
	}
	stage("pipeline complete: issue published")
	return nil
}

// ingestStats summarises a single ingest pass.
type ingestStats struct {
	total  int
	ok     int
	failed int
}

// ingestAll loads the enabled sources for domainID from the store, builds
// each one through the ingest factory registry, then fetches all of them
// concurrently with a bounded total timeout. Individual source failures
// are logged but do not abort the whole pipeline.
func ingestAll(ctx context.Context, s store.Store, domainID string, perSourceTimeout time.Duration) ([]*store.RawItem, ingestStats, error) {
	stats := ingestStats{}
	sources, err := s.ListEnabledSources(ctx, domainID)
	if err != nil {
		return nil, stats, fmt.Errorf("list enabled sources: %w", err)
	}
	stats.total = len(sources)
	if len(sources) == 0 {
		return nil, stats, errors.New("no enabled sources in database — run `briefing seed` first")
	}

	type result struct {
		sourceName string
		items      []*store.RawItem
		err        error
	}

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		results []result
	)

	sem := make(chan struct{}, 8) // cap concurrency

	for _, src := range sources {
		wg.Add(1)
		sem <- struct{}{}
		go func(row *store.Source) {
			defer wg.Done()
			defer func() { <-sem }()

			adapter, err := ingest.Build(row)
			if err != nil {
				mu.Lock()
				results = append(results, result{sourceName: row.Name, err: fmt.Errorf("build: %w", err)})
				mu.Unlock()
				return
			}

			subCtx, cancel := context.WithTimeout(ctx, perSourceTimeout)
			defer cancel()

			items, err := adapter.Fetch(subCtx)
			mu.Lock()
			results = append(results, result{sourceName: row.Name, items: items, err: err})
			mu.Unlock()
		}(src)
	}
	wg.Wait()

	var allItems []*store.RawItem
	for _, r := range results {
		if r.err != nil {
			stats.failed++
			fmt.Printf("[WARN] ingest %s: %v\n", r.sourceName, r.err)
			continue
		}
		stats.ok++
		fmt.Printf("[ingest] %s → %d items\n", r.sourceName, len(r.items))
		allItems = append(allItems, r.items...)
	}

	return allItems, stats, nil
}

// filterByWindow keeps only items whose PublishedAt (or FetchedAt fallback)
// is after cutoff. Items with zero PublishedAt and zero FetchedAt are kept
// conservatively (we cannot prove they are old).
func filterByWindow(items []*store.RawItem, cutoff time.Time) []*store.RawItem {
	out := make([]*store.RawItem, 0, len(items))
	for _, it := range items {
		if it == nil {
			continue
		}
		ts := it.PublishedAt
		if ts.IsZero() {
			ts = it.FetchedAt
		}
		if ts.IsZero() || ts.After(cutoff) {
			out = append(out, it)
		}
	}
	return out
}

// generateDailySummary asks the LLM for a 3-line summary. We call the
// OpenAI-compatible chat/completions endpoint directly here rather than
// reaching into the generate package because the prompt is one-off and
// adding a full interface method would bloat generate with a feature only
// used once in the pipeline.
func generateDailySummary(ctx context.Context, llmCfg config.LLMConfig, items []*store.IssueItem) (string, error) {
	if len(items) == 0 {
		return "", nil
	}

	const systemPrompt = `你是一名资深 AI 行业编辑，擅长写"新闻大字报"风格的头版标题党。请根据今日所有候选新闻标题，提炼出 3 行"今日头条大字报"。

要求：
- 每行就是一条重磅新闻的标题党句子，强冲击力、强对比、强吸睛感
- 每行 20-45 个汉字
- 必须点出具体公司/产品名（DeepSeek、Anthropic、OpenAI、Claude 等），不能虚写
- 纯文本，无序号，无 markdown，无 emoji
- 可以用"重磅"、"震撼"、"突袭"、"颠覆"、"炸裂"、"屠榜"等带情绪的词增加趣味性，但要**克制**：每行最多一个这类词
- 关键是靠事实本身制造冲击力（具体数字、具体动作、具体对比），形容词只是锦上添花
- 每行内部可以用逗号把两三个事件拼在一起，制造信息密度
- 直接输出这 3 行，不加任何解释或前后缀

好的示例：
DeepSeek V4 凌晨突袭暗更，Anthropic 托管 Agent 定价 0.08 美元/小时炸裂 Agent 成本
OpenAI 悄然移除安全关停机制，Aristotle AI 攻克 91% 厄多斯数学难题震撼学界
Meta 首个前沿模型 Muse Spark 转闭源，Claude Sonnet 4.6 一天连发编码 Agent 重磅

坏的示例（不要这样写）：
今日 AI 行业重大更新，多个产品发布 ← 太虚，无事件
科技巨头集体行动，震撼 AI 领域 ← 没具体事实，纯形容词堆砌
让人眼前一亮的多模态突破 ← 没公司没数字`

	var titles []string
	for i, it := range items {
		if i >= 30 {
			break
		}
		if it != nil && strings.TrimSpace(it.Title) != "" {
			titles = append(titles, strings.TrimSpace(it.Title))
		}
	}
	userPrompt := "今日所有条目标题:\n" + strings.Join(titles, "\n")

	reqBody := map[string]any{
		"model": llmCfg.Model,
		"messages": []map[string]any{
			{"role": "system", "content": systemPrompt},
			{"role": "user", "content": userPrompt},
		},
		"temperature": llmCfg.Temperature,
		"max_tokens":  500,
	}
	buf, err := json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("marshal summary request: %w", err)
	}

	callCtx, cancel := context.WithTimeout(ctx, llmCfg.LLMTimeout())
	defer cancel()

	apiURL := strings.TrimRight(llmCfg.BaseURL, "/") + "/v1/chat/completions"
	req, err := http.NewRequestWithContext(callCtx, http.MethodPost, apiURL, bytes.NewReader(buf))
	if err != nil {
		return "", fmt.Errorf("new summary request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+llmCfg.APIKey)

	hc := &http.Client{Timeout: llmCfg.LLMTimeout()}
	resp, err := hc.Do(req)
	if err != nil {
		return "", fmt.Errorf("summary http: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("summary read: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet := string(body)
		if len(snippet) > 500 {
			snippet = snippet[:500]
		}
		return "", fmt.Errorf("summary http %d: %s", resp.StatusCode, snippet)
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", fmt.Errorf("summary unmarshal: %w", err)
	}
	if len(parsed.Choices) == 0 {
		return "", errors.New("summary: empty choices")
	}
	return strings.TrimSpace(parsed.Choices[0].Message.Content), nil
}

// postSlackPayload sends payload to webhookURL and returns a Delivery
// record reflecting the outcome. Never returns an error — the Delivery
// status field carries success / failure.
func postSlackPayload(ctx context.Context, channel, webhookURL string, payload []byte, issueID int64) *store.Delivery {
	now := time.Now()
	d := &store.Delivery{
		IssueID: issueID,
		Channel: channel,
		Target:  webhookURL,
		SentAt:  now,
	}
	if webhookURL == "" {
		d.Status = store.DeliveryStatusSkipped
		d.ResponseJSON = `{"reason":"webhook url empty"}`
		return d
	}

	subCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(subCtx, http.MethodPost, webhookURL, bytes.NewReader(payload))
	if err != nil {
		d.Status = store.DeliveryStatusFailed
		d.ResponseJSON = fmt.Sprintf(`{"error":"build request: %s"}`, err.Error())
		return d
	}
	req.Header.Set("Content-Type", "application/json")

	hc := &http.Client{Timeout: 15 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		d.Status = store.DeliveryStatusFailed
		d.ResponseJSON = fmt.Sprintf(`{"error":%q}`, err.Error())
		return d
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)
	d.ResponseJSON = string(body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		d.Status = store.DeliveryStatusSent
	} else {
		d.Status = store.DeliveryStatusFailed
	}
	return d
}

// postAlert posts a plain-text alert message to webhookURL. Used when gate
// fails in auto/prod mode — we never want to stay silent about quality fails.
func postAlert(ctx context.Context, webhookURL string, body []byte) error {
	if webhookURL == "" {
		return errors.New("alert: empty webhook")
	}
	subCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(subCtx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	hc := &http.Client{Timeout: 10 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// buildGateFailAlert formats a Slack plain-text alert describing why the
// gate rejected today's issue.
func buildGateFailAlert(issue *store.Issue, r *gate.Report) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🚨 briefing-v3 %s 质量不达标,正式频道已跳过\n", issue.IssueDate.Format("2006-01-02"))
	fmt.Fprintf(&b, "• 条目数 %d | 非空 section %d | 洞察字数 %d\n",
		r.ItemCount, r.SectionCount, r.InsightChars)
	fmt.Fprintf(&b, "• 行业洞察 %d 条 | 启发 %d 条 | 独立源 %d 个\n",
		r.IndustryBullets, r.TakeawayBullets, r.SourceDomainCount)
	b.WriteString("未通过原因:\n")
	for _, reason := range r.Reasons {
		fmt.Fprintf(&b, "  - %s\n", reason)
	}
	return b.String()
}

// extractTopHeadline picks a short, punchy headline for the cover image.
// Strategy: use the first sentence of the summary (if available); otherwise
// use the title of the first issue item.
func extractTopHeadline(items []*store.IssueItem, summary string) string {
	summary = strings.TrimSpace(summary)
	if summary != "" {
		// Split on line breaks; prefer the first non-empty line.
		for _, line := range strings.Split(summary, "\n") {
			line = strings.TrimSpace(line)
			if line != "" {
				if len([]rune(line)) > 30 {
					line = string([]rune(line)[:30])
				}
				return line
			}
		}
	}
	for _, it := range items {
		if it != nil && strings.TrimSpace(it.Title) != "" {
			t := strings.TrimSpace(it.Title)
			// Strip leading numbering + markdown bold markers that compose
			// might have left on the raw title (defensive).
			t = strings.TrimLeft(t, "0123456789. ")
			t = strings.TrimPrefix(t, "**")
			t = strings.TrimSuffix(t, "**")
			if len([]rune(t)) > 30 {
				t = string([]rune(t)[:30])
			}
			return t
		}
	}
	return "AI 资讯日报"
}

// countSourceDomains returns the number of distinct host names across the
// SourceURLsJSON of every IssueItem. Used to populate issue.SourceCount.
func countSourceDomains(items []*store.IssueItem) int {
	seen := make(map[string]struct{})
	for _, it := range items {
		if it == nil || it.SourceURLsJSON == "" {
			continue
		}
		var urls []string
		if err := json.Unmarshal([]byte(it.SourceURLsJSON), &urls); err != nil {
			continue
		}
		for _, u := range urls {
			if host := domainFromURL(u); host != "" {
				seen[host] = struct{}{}
			}
		}
	}
	return len(seen)
}

// domainFromURL returns the host name of raw (or empty string on parse error).
func domainFromURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ""
	}
	return strings.ToLower(u.Host)
}

// stableSortItemsBySectionSeq ensures deterministic ordering when the upstream
// store returns items in insertion order. The renderer already sorts, so this
// is purely defensive for any downstream consumer inspecting the slice.
func stableSortItemsBySectionSeq(items []*store.IssueItem) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].Section != items[j].Section {
			return items[i].Section < items[j].Section
		}
		return items[i].Seq < items[j].Seq
	})
}

// enrichItemsWithMedia walks every IssueItem, inspects its
// SourceURLsJSON, and tries to extract a hero image (og:image) and
// optional video (og:video / <video>) from the original article URLs
// via internal/mediaextract. When found, the hero media is appended
// to BodyMD as a markdown image (![alt](url)) and a custom
// [[VIDEO:url]] placeholder that render.miniMarkdownToHTML knows how
// to turn into a real <video> tag.
//
// Returns the number of items that got any media at all.
//
// Concurrency: we collect ALL source URLs across all items into one
// flat slice and run a single bounded-concurrency batch. This keeps
// the wall-clock time down to (max_urls / concurrency) * per-request
// timeout even when a run produces 20+ items.
func enrichItemsWithMedia(ctx context.Context, items []*store.IssueItem) int {
	if len(items) == 0 {
		return 0
	}

	ex := mediaextract.New()

	// Flatten all source URLs while remembering which item owns which.
	type urlRef struct {
		itemIdx int
		url     string
	}
	var allRefs []urlRef
	for i, it := range items {
		if it == nil || strings.TrimSpace(it.SourceURLsJSON) == "" {
			continue
		}
		var urls []string
		if err := json.Unmarshal([]byte(it.SourceURLsJSON), &urls); err != nil {
			continue
		}
		// Cap at 3 URLs per item so we do not spam a site with
		// too many requests.
		for j, u := range urls {
			if j >= 3 {
				break
			}
			u = strings.TrimSpace(u)
			if u == "" {
				continue
			}
			allRefs = append(allRefs, urlRef{itemIdx: i, url: u})
		}
	}

	if len(allRefs) == 0 {
		return 0
	}

	// Batch extract.
	urls := make([]string, len(allRefs))
	for i, r := range allRefs {
		urls[i] = r.url
	}
	results := ex.ExtractBatch(ctx, urls, 8)

	// Collate per-item: pick the first image and first video we find
	// across that item's URL set.
	type collected struct {
		image string
		video string
		alt   string
	}
	byItem := make(map[int]*collected)
	for i, ref := range allRefs {
		m := results[i]
		if m == nil {
			continue
		}
		c, ok := byItem[ref.itemIdx]
		if !ok {
			c = &collected{}
			byItem[ref.itemIdx] = c
		}
		if c.image == "" && m.HasImage() {
			c.image = m.ImageURL
			if strings.TrimSpace(m.AltText) != "" {
				c.alt = m.AltText
			}
		}
		if c.video == "" && m.HasVideo() {
			c.video = m.VideoURL
		}
	}

	// Apply back to IssueItem.BodyMD.
	enriched := 0
	for idx, c := range byItem {
		if c == nil || (c.image == "" && c.video == "") {
			continue
		}
		it := items[idx]
		if it == nil {
			continue
		}
		alt := c.alt
		if alt == "" {
			alt = strings.TrimSpace(it.Title)
		}
		// Strip square brackets and parens from alt so we do not
		// accidentally break the markdown image syntax.
		alt = strings.ReplaceAll(alt, "[", " ")
		alt = strings.ReplaceAll(alt, "]", " ")
		alt = strings.ReplaceAll(alt, "(", " ")
		alt = strings.ReplaceAll(alt, ")", " ")
		alt = strings.TrimSpace(alt)

		var b strings.Builder
		b.WriteString(strings.TrimRight(it.BodyMD, "\n"))
		b.WriteString("\n\n")
		if c.image != "" {
			fmt.Fprintf(&b, "![%s](%s)\n", alt, c.image)
		}
		if c.video != "" {
			fmt.Fprintf(&b, "\n[[VIDEO:%s]]\n", c.video)
		}
		it.BodyMD = b.String()
		enriched++
	}

	return enriched
}

// renderInfoCardPNG invokes the Python PIL renderer script via stdin.
// mode is either "item" or "header". card is the Go struct that will
// be JSON-marshalled and fed on stdin. outputPath is where the PNG
// gets written. Subprocess bounded to 30s.
func renderInfoCardPNG(ctx context.Context, mode string, card any, outputPath string) error {
	jsonBytes, err := json.Marshal(card)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	subCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(subCtx, "python3", "scripts/gen_info_card.py",
		"--mode", mode,
		"--output", outputPath,
	)
	cmd.Stdin = bytes.NewReader(jsonBytes)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := stderr.String()
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		return fmt.Errorf("python %s: %w (stderr: %s)", mode, err, msg)
	}
	return nil
}

// writeDailyMarkdown persists the rendered markdown to daily/YYYY-MM-DD.md.
// Used so we always have a flat-text copy for git history and manual review,
// mirroring the upstream `book` branch that stores one .md per day.
func writeDailyMarkdown(date time.Time, md string) error {
	dir := "daily"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, date.Format("2006-01-02")+".md")
	return os.WriteFile(path, []byte(md), 0o644)
}
