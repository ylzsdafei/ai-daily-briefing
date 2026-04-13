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
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"briefing-v3/internal/classify"
	"briefing-v3/internal/compose"
	"briefing-v3/internal/config"
	"briefing-v3/internal/gate"
	"briefing-v3/internal/generate"
	"briefing-v3/internal/illustration"
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

	// --- 4b. Cross-run dedup --------------------------------------------
	// v1.0.0: 用户反馈 "我希望每一次都是新的信息,而不是重复的内容收到3次".
	// 维护 data/sent_urls.txt 文件作为 URL set, filter 阶段排除已经在历史
	// briefing 中推送过的 URL. 这样即使同一天多次 run, 每次也都是全新内容.
	// 实现 fail-soft: dedup 是优化项, 任何错误都不阻塞 pipeline.
	sentURLs := loadSentURLs()
	if len(sentURLs) > 0 {
		beforeDedup := len(filtered)
		filtered = dedupRawItemsBySent(filtered, sentURLs)
		stage(fmt.Sprintf("dedup: %d → %d items (skipped %d already pushed in past runs)",
			beforeDedup, len(filtered), beforeDedup-len(filtered)))
	}
	if len(filtered) == 0 {
		return errors.New("dedup: every item in window already published — nothing new to push (consider clearing data/sent_urls.txt)")
	}

	// v1.0.1: title-based dedup (same news from different sources).
	sentTitles := loadSentTitles()
	if len(sentTitles) > 0 {
		beforeTitleDedup := len(filtered)
		filtered = dedupRawItemsByTitle(filtered, sentTitles)
		if dropped := beforeTitleDedup - len(filtered); dropped > 0 {
			stage(fmt.Sprintf("title-dedup: %d → %d items (skipped %d similar titles)",
				beforeTitleDedup, len(filtered), dropped))
		}
	}

	// --- 5a. Build sourceID → category map for rank + classify ---------
	// v1.0.0 INTERFACE CHANGE (T2/C-stage): rank + classify now take a
	// sourceCategories map so they can apply per-category quota and
	// rule-first pre-classification. The authoritative category field
	// lives in config/ai.yaml and is persisted into sources.config_json
	// by seedCommand; ListEnabledSources parses it back out onto
	// store.Source.Category.
	sourceRows, err := s.ListEnabledSources(ctx, gf.domain)
	if err != nil {
		return fmt.Errorf("list sources for category map: %w", err)
	}
	sourceCategories := make(map[int64]string, len(sourceRows))
	for _, sr := range sourceRows {
		if sr == nil {
			continue
		}
		sourceCategories[sr.ID] = sr.Category
	}

	// --- 5. Rank (LLM quality scoring) ----------------------------------
	stage("rank: calling LLM quality scorer")
	ranker, err := rank.New(rank.Config{
		BaseURL:          cfg.LLM.BaseURL,
		APIKey:           cfg.LLM.APIKey,
		Model:            cfg.LLM.Model,
		Timeout:          cfg.LLM.LLMTimeout(),
		PerCategoryQuota: cfg.Rank.PerCategoryQuota,
	})
	if err != nil {
		return fmt.Errorf("rank new: %w", err)
	}
	ranked, err := ranker.Rank(ctx, filtered, sourceCategories)
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

	// --- 6. Classify (rule pre-classify + LLM binary disambiguation) ----
	stage("classify: rule pre-classify + LLM news binary")
	classifier, err := classify.New(classify.Config{
		BaseURL: cfg.LLM.BaseURL,
		APIKey:  cfg.LLM.APIKey,
		Model:   cfg.LLM.Model,
		Timeout: cfg.LLM.LLMTimeout(),
	})
	if err != nil {
		return fmt.Errorf("classify new: %w", err)
	}
	sectioned, err := classifier.Classify(ctx, rankedRaws, sourceCategories)
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
	// INTERFACE CHANGE (T2/C3): Compose() now returns (items, failedSections, err).
	// failedSections drives the gate Warn downgrade in stage 14.
	// T3 will take over refinement of how failedSections is surfaced to
	// the render + Slack payload.
	issueItems, composeFailedSections, err := composer.Compose(ctx, issueID, sectioned, composeSections, summarizer)
	if err != nil {
		return fmt.Errorf("compose: %w", err)
	}
	if len(composeFailedSections) > 0 {
		stage(fmt.Sprintf("compose: %d section(s) degraded: %s",
			len(composeFailedSections), strings.Join(composeFailedSections, ",")))
	}
	stage(fmt.Sprintf("compose: produced %d issue items", len(issueItems)))

	// --- 7b. Extract hero image/video from source URLs (fallback only) ----
	// This is the fallback media path. The primary path is infocard
	// (editorial-style PIL info cards) built below from LLM-distilled
	// structured JSON. mediaextract only runs to give items a hotlink
	// image/video IF the info-card generation later fails.
	if gf.noImages {
		stage("media: --no-images → skipping mediaextract")
	} else {
		stage("media: extracting fallback hero image/video from source URLs")
		mediaFound := enrichItemsWithMedia(ctx, issueItems)
		stage(fmt.Sprintf("media: %d items got a fallback hero image/video", mediaFound))
	}

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
	// v1.0.0: 之前是 hard stop, 上游 LLM 一次 502 就让整条 pipeline 退出.
	// 改为 fail-soft: LLM 临时挂掉就用本地兜底 summary (取前 3 个 high-quality
	// item 的标题), pipeline 继续走完, 早报照样推送, 只是 summary 行内容降级.
	stage("summary: generating 3-line daily summary")
	summary, err := generateDailySummary(ctx, cfg.LLM, issueItems)
	if err != nil {
		fmt.Printf("[WARN] summary: %v — falling back to title-based local summary\n", err)
		summary = buildFallbackSummary(issueItems)
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
	var headerCardPNGRel string
	if gf.noImages {
		stage("infocard: --no-images → skipping LLM card generation")
	} else {
	stage("infocard: generating editorial info-card JSON via LLM")
	icGen, icErr := infocard.New(infocard.Config{
		BaseURL:    cfg.LLM.BaseURL,
		APIKey:     cfg.LLM.APIKey,
		Model:      cfg.LLM.Model,
		MaxRetries: 3,
		Timeout:    cfg.LLM.LLMTimeout(),
	})
	if icErr != nil {
		fmt.Printf("[WARN] infocard new: %v — falling back to mediaextract images only\n", icErr)
	} else {
		// compose.Seq restarts per section (1..N), so multiple items across
		// different sections can share Seq=1,2,3… Passing those to the LLM
		// would collapse all cards with the same seq onto the same PNG
		// filename. Build a UID-remapped shadow slice where every item has
		// a globally-unique Seq (1..totalItems), pass the shadows to the
		// infocard LLM, then match the returned cards back via UID.
		// v1.0.1 L1: only pass top 12 items to infocard LLM to reduce
		// prompt size and avoid 6-minute timeouts. Items are already
		// rank-ordered, so the first 12 are the highest quality.
		infocardSrc := issueItems
		if len(infocardSrc) > 12 {
			infocardSrc = infocardSrc[:12]
		}
		shadowItems := make([]*store.IssueItem, 0, len(infocardSrc))
		uidToItem := make(map[int]*store.IssueItem, len(infocardSrc))
		for i, it := range infocardSrc {
			if it == nil {
				continue
			}
			shadow := *it
			shadow.Seq = i + 1
			shadowItems = append(shadowItems, &shadow)
			uidToItem[shadow.Seq] = it
		}

		header, cards, err := icGen.Generate(ctx, shadowItems, summary)
		if err == nil && header != nil {
			// LLM 成功. 但 LLM prompt 还是旧 schema (6 stories / 3 numbers /
			// 单行 sub_headline), 跟新 PIL newspaper layout (11 stories / 6
			// numbers / multi-line L2+L3) 不匹配, MORE STORIES / KEY NUMBERS
			// 后排会空着. 用 buildFallbackHeaderCard 的字段补齐缺失部分,
			// LLM 的字段优先, fallback 只补 LLM 没生成的.
			enrichLLMHeader(header, issueItems, summary, issue.IssueNumber, date.Format("2006-01-02"))
		}
		if err != nil {
			fmt.Printf("[WARN] infocard generate: %v — using local fallback header\n", err)
			// v1.0.0 fail-soft: LLM 上游一直 6 分钟超时, 不能因此让大字报
			// 永远是旧图. 用本地构造器从 issueItems + summary 直接拼出
			// HeaderCard, 喂给同一个 PIL 渲染脚本, 保证大字报永远是当天的.
			fallbackHeader := buildFallbackHeaderCard(issueItems, summary, issue.IssueNumber, date.Format("2006-01-02"))
			cardDir := filepath.Join("data", "images", "cards", date.Format("2006-01-02"))
			if mkErr := os.MkdirAll(cardDir, 0o755); mkErr != nil {
				fmt.Printf("[WARN] infocard fallback mkdir: %v\n", mkErr)
			} else {
				headerPath := filepath.Join(cardDir, "header.png")
				if rdErr := renderInfoCardPNG(ctx, "header", fallbackHeader, headerPath); rdErr != nil {
					fmt.Printf("[WARN] infocard fallback render: %v\n", rdErr)
				} else {
					headerCardPNGRel = fmt.Sprintf("../data/images/cards/%s/header.png", date.Format("2006-01-02"))
					stage(fmt.Sprintf("infocard: fallback header PNG written to %s", headerPath))
				}
			}
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
	} // end of: if gf.noImages { ... } else { ... }

	// --- 10c. Defensive scrub of any media markdown from BodyMD ---------
	// When --no-images is set we must be 100% sure no image or video
	// tag survives into the Slack mrkdwn (which does not support image
	// markdown and would render `![alt](url)` as an ugly literal string).
	// This also catches any image that the compose LLM itself injected
	// into a body from the raw source content.
	if gf.noImages {
		stripMediaFromIssueItems(issueItems)
		stage("scrub: stripped image/video markdown from all items (--no-images)")
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

	// --- 11b. Write Hextra content markdown (v1.0.0 D2) ---------------
	// When HEXTRA_SITE_DIR is set, write the canonical markdown (wrapped
	// in a Hextra frontmatter block) under
	// {HEXTRA_SITE_DIR}/content/cn/YYYY-MM/YYYY-MM-DD.md. A write failure
	// is non-fatal — Slack publishing still proceeds. T3/v1.0.0 D2.
	if hextraDir := os.Getenv("HEXTRA_SITE_DIR"); hextraDir != "" {
		hugoPath, hugoErr := render.WriteHugoPost(hextraDir, issue, issueItems, insight, renderSecs)
		if hugoErr != nil {
			fmt.Printf("[WARN] hugo write post failed: %v (continuing)\n", hugoErr)
		} else {
			stage(fmt.Sprintf("hugo: wrote %s", hugoPath))
		}
	}

	// --- 11c. Hugo build static site (v1.0.0 D4) ----------------------
	// When HUGO_BIN + HEXTRA_SITE_DIR are both set, rebuild the Hextra
	// site so the new content/*.md is picked up and published at
	// {HEXTRA_SITE_DIR}/public/YYYY-MM/YYYY-MM-DD/. The build runs with a
	// 60s timeout and its failure is logged but does NOT block the Slack
	// publish — static-site refresh is always lower priority than the
	// outbound Slack notification. The Go binary lives at /usr/local/go
	// (T1 discovery), which Hextra's Hugo module resolution requires on
	// PATH; without that, `hugo` errors out with "binary with name 'go'
	// not found in PATH".
	if hugoBin := os.Getenv("HUGO_BIN"); hugoBin != "" {
		if hextraDir := os.Getenv("HEXTRA_SITE_DIR"); hextraDir != "" {
			buildCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			cmd := exec.CommandContext(buildCtx, hugoBin, "--source", hextraDir, "--minify")
			cmd.Env = append(os.Environ(), "PATH=/usr/local/go/bin:"+os.Getenv("PATH"))
			if out, err := cmd.CombinedOutput(); err != nil {
				fmt.Printf("[WARN] hugo build failed: %v\n%s (continuing)\n", err, string(out))
			} else {
				stage("hugo: build complete")
			}
			cancel()
		}
	}

	// --- 12. Generate headline image (local PNG only; Slack image_url
	//         stays empty until we have a public image host) ------------
	var headlineImageURL string
	headlineText := extractTopHeadline(issueItems, summary)
	if gf.noImages {
		stage("image: --no-images → skipping headline PNG")
	} else if cfg.Image.Enabled {
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
	if gf.noImages {
		headlineRelForHTML = ""
	} else if headlineRelForHTML == "" && cfg.Image.Enabled {
		// The PNG lives at data/images/YYYY-MM-DD.png; docs/*.html sits
		// one level deep under briefing-v3/, so the relative href is
		// ../data/images/... which browsers open correctly via file://.
		headlineRelForHTML = fmt.Sprintf("../data/images/%s.png", date.Format("2006-01-02"))
	}
	// v1.0.0 D3: Hextra migration deprecated docs/*.html. The legacy HTML
	// path is now provided by the new Hextra static site (11b + 11c above).
	// The three calls below are kept commented out as a rollback point:
	// should v1.0.1 need to revert to the self-written HTML template, we
	// simply uncomment this block and re-enable briefing-serve.service on
	// port 8080. Do NOT remove the referenced helpers from render/html.go —
	// they are preserved for the same reason.
	_ = headlineRelForHTML // silence unused-var when the block is commented
	/*
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
	*/

	// --- 13. Build RenderedIssue for downstream render/publish ---------
	// ReportURL points at the local HTML page via an absolute file:// URI
	// so that Slack buttons at least identify the right file during
	// development. Once GitHub Pages (or another host) is configured, set
	// an env var BRIEFING_REPORT_URL_BASE to override this with a public
	// URL. Example: https://ylzsdafei.github.io/briefing-v3/{{DATE}}.html
	// v1.0.0 D7a: support both {{DATE}} (YYYY-MM-DD) and {{YEARMONTH}}
	// (YYYY-MM) placeholders so operators can point report URLs at the
	// Hextra-style path /YYYY-MM/YYYY-MM-DD/ instead of the legacy
	// docs/YYYY-MM-DD.html path. Both placeholders are replaced; unknown
	// tokens pass through unchanged.
	reportURL := fmt.Sprintf("file:///root/briefing-v3/docs/%s.html", date.Format("2006-01-02"))
	if base := os.Getenv("BRIEFING_REPORT_URL_BASE"); base != "" {
		reportURL = strings.ReplaceAll(base, "{{DATE}}", date.Format("2006-01-02"))
		reportURL = strings.ReplaceAll(reportURL, "{{YEARMONTH}}", date.Format("2006-01"))
		// v1.0.0: 三级 sidebar tree 需要 /YYYY/YYYY-MM/YYYY-MM-DD/ URL
		reportURL = strings.ReplaceAll(reportURL, "{{YEAR}}", date.Format("2006"))
	}

	// --- 14. Hard quality gate -----------------------------------------
	// INTERFACE CHANGE (T2/C4): gate.Check() now takes failedSections
	// and totalSections, and returns Warn + Warnings alongside Pass +
	// Reasons. Outcomes: Pass=true → green; Pass=false,Warn=true → yellow
	// "质量待审"; Pass=false,Warn=false → hard fail. v1.0.0 D7b wires
	// Warn/Warnings/FailedSections into RenderedIssue below so the Slack
	// renderer can surface the degraded state visually.
	stage("gate: checking quality rules (tri-state)")
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
	report := g.Check(issue, issueItems, insight, composeFailedSections, len(cfg.Sections))

	// --- 13 (post-gate). Build RenderedIssue for downstream render/publish.
	// Moved *after* the gate so v1.0.0 D7b can feed the Warn/Warnings/
	// FailedSections into the Slack renderer in the same construction step.
	rendered := &publish.RenderedIssue{
		Issue:            issue,
		Items:            issueItems,
		Insight:          insight,
		HeadlineImageURL: headlineImageURL,
		SectionsMarkdown: sectionsMD,
		DateZH:           render.FormatDateZH(issue),
		ReportURL:        reportURL,
		QualityWarn:      report.Warn,
		QualityWarnings:  report.Warnings,
		FailedSections:   report.FailedSections,
	}
	stage(fmt.Sprintf("gate: pass=%v warn=%v items=%d sections=%d insightChars=%d industry=%d takeaways=%d domains=%d failedSections=%d",
		report.Pass, report.Warn, report.ItemCount, report.SectionCount, report.InsightChars,
		report.IndustryBullets, report.TakeawayBullets, report.SourceDomainCount,
		len(report.FailedSections)))
	if !report.Pass {
		if report.Warn {
			for _, w := range report.Warnings {
				fmt.Printf("[GATE WARN] %s\n", w)
			}
			// v1.0.1: gate warn → auto-retry via systemd.
			// BRIEFING_RETRY_COUNT is set by the retry service units.
			retryCount, _ := strconv.Atoi(os.Getenv("BRIEFING_RETRY_COUNT"))
			if retryCount < 3 {
				// Exit 1 → systemd OnFailure triggers next retry timer.
				// Don't push to any channel — let the retry produce better content.
				return fmt.Errorf("gate warn (retry %d/3), will retry: %s",
					retryCount, strings.Join(report.Warnings, "; "))
			}
			// 3rd retry still warned → treat as pass, push normally.
			stage(fmt.Sprintf("gate warn: retry %d/3 exhausted, treating as pass", retryCount))
			report.Pass = true
			report.Warn = false
		} else {
			for _, reason := range report.Reasons {
				fmt.Printf("[GATE FAIL] %s\n", reason)
			}
		}
	}

	// --- 15. Build the Slack payload once (shared between test + prod) -
	slackPayload, err := render.BuildSlackPayload(rendered)
	if err != nil {
		return fmt.Errorf("render slack payload: %w", err)
	}

	// Persist the exact bytes we are about to POST so `briefing promote`
	// can re-send them verbatim to the prod webhook later. We intentionally
	// write before dry-run branches so even a dry-run snapshot is usable
	// for later inspection.
	if err := savePayloadSnapshot(date, slackPayload); err != nil {
		fmt.Printf("[WARN] save payload snapshot: %v\n", err)
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
			if shouldPostGateAlert(report) {
				alertMsg := buildGateFailAlert(issue, report)
				alertBody, _ := json.Marshal(map[string]any{"text": alertMsg})
				_ = postAlert(ctx, cfg.Slack.TestWebhook, alertBody)
			}
			if gateFailureBlocksRun(gf.target, report) {
				return fmt.Errorf("gate failed, prod channel skipped: %s", gateFailureDetail(report))
			}
			stage("publish: gate warn → alert sent to test, continuing to prod publish")
		}
		if issues := prodPublishIssues(ctx, rendered); len(issues) > 0 {
			alertMsg := buildProdReadinessAlert(issue, issues)
			alertBody, _ := json.Marshal(map[string]any{"text": alertMsg})
			_ = postAlert(ctx, cfg.Slack.TestWebhook, alertBody)
			return fmt.Errorf("prod readiness failed: %s", strings.Join(issues, "; "))
		}
		stage("publish: gate passed/warned → posting to Slack prod channel")
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

	// --- 18b. Persist sent URLs for cross-run dedup --------------------
	// 把这次推送的所有 source URL 加入 sent set, 下次 run 不再选这些条目.
	// 用户原话: "我希望每一次都是新的信息". fail-soft: 写入失败 log 警告
	// 但不返回 error (推送已经成功, dedup 优化失败不影响本次结果).
	if newSent := collectIssueItemSourceURLs(issueItems); len(newSent) > 0 {
		appendSentURLs(newSent)
		stage(fmt.Sprintf("dedup: persisted %d new URLs to sent set", len(newSent)))
	}

	// v1.0.1: persist sent titles for title-based dedup.
	var newTitles []string
	for _, it := range issueItems {
		if it != nil && strings.TrimSpace(it.Title) != "" {
			newTitles = append(newTitles, it.Title)
		}
	}
	if len(newTitles) > 0 {
		appendSentTitles(newTitles)
		stage(fmt.Sprintf("title-dedup: persisted %d new titles", len(newTitles)))
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

// Regexes used by extractDateFromText to recover a real publication date
// from adapter output when the upstream source embedded the date in the
// title or body instead of a machine-readable field.
//
// Order matters: we try the high-specificity patterns first. Lower-case
// month abbreviations are handled via strings.ToLower() in the function.
var (
	// English: "Feb 17, 2026", "February 17, 2026", "Feb17, 2026" (no space).
	titleDateReEN = regexp.MustCompile(`(?i)(jan(?:uary)?|feb(?:ruary)?|mar(?:ch)?|apr(?:il)?|may|jun(?:e)?|jul(?:y)?|aug(?:ust)?|sep(?:tember)?|oct(?:ober)?|nov(?:ember)?|dec(?:ember)?)[a-z]*\s*(\d{1,2}),?\s*(\d{4})`)
	// ISO: 2026-02-17 or 2026/02/17
	titleDateReISO = regexp.MustCompile(`(\d{4})[-/](\d{1,2})[-/](\d{1,2})`)
	// Chinese: 2026年2月17日 / 2026 年 2 月 17 日
	titleDateReZH = regexp.MustCompile(`(\d{4})\s*年\s*(\d{1,2})\s*月\s*(\d{1,2})\s*日`)
)

var monthAbbrToNum = map[string]time.Month{
	"jan": time.January, "feb": time.February, "mar": time.March,
	"apr": time.April, "may": time.May, "jun": time.June,
	"jul": time.July, "aug": time.August, "sep": time.September,
	"oct": time.October, "nov": time.November, "dec": time.December,
}

// extractDateFromText parses a dateline out of arbitrary text.
// Returns (date, true) on success, (zero, false) on failure.
// The returned date is set to 00:00 UTC of the matched day.
func extractDateFromText(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	now := time.Now().UTC()
	validYear := func(y int) bool { return y >= 2000 && y <= now.Year()+1 }
	validMonth := func(m int) bool { return m >= 1 && m <= 12 }
	validDay := func(d int) bool { return d >= 1 && d <= 31 }

	if m := titleDateReEN.FindStringSubmatch(s); len(m) == 4 {
		key := strings.ToLower(m[1])
		if len(key) > 3 {
			key = key[:3]
		}
		mon, ok := monthAbbrToNum[key]
		day, _ := strconv.Atoi(m[2])
		year, _ := strconv.Atoi(m[3])
		if ok && validYear(year) && validDay(day) {
			return time.Date(year, mon, day, 0, 0, 0, 0, time.UTC), true
		}
	}
	if m := titleDateReISO.FindStringSubmatch(s); len(m) == 4 {
		year, _ := strconv.Atoi(m[1])
		mn, _ := strconv.Atoi(m[2])
		day, _ := strconv.Atoi(m[3])
		if validYear(year) && validMonth(mn) && validDay(day) {
			return time.Date(year, time.Month(mn), day, 0, 0, 0, 0, time.UTC), true
		}
	}
	if m := titleDateReZH.FindStringSubmatch(s); len(m) == 4 {
		year, _ := strconv.Atoi(m[1])
		mn, _ := strconv.Atoi(m[2])
		day, _ := strconv.Atoi(m[3])
		if validYear(year) && validMonth(mn) && validDay(day) {
			return time.Date(year, time.Month(mn), day, 0, 0, 0, 0, time.UTC), true
		}
	}
	return time.Time{}, false
}

// filterByWindow keeps only items whose effective publication date is
// after cutoff. "Effective" means: if the title (or content preview)
// contains an explicit dateline, we trust that over whatever PublishedAt
// the adapter set, because multiple adapters currently fall back to
// fetch time when they cannot parse the real date — a pattern that lets
// 2-month-old press releases leak into a 24h briefing.
//
// Items with NO recoverable date (zero PublishedAt AND no dateline in
// title/content) are DROPPED rather than kept. The prior "keep on
// unknown" policy produced false positives; the user explicitly asked
// for quality over recall.
func filterByWindow(items []*store.RawItem, cutoff time.Time) []*store.RawItem {
	out := make([]*store.RawItem, 0, len(items))
	var fallbackHits, droppedStale, droppedUnknown int
	for _, it := range items {
		if it == nil {
			continue
		}
		// Try to recover a more reliable date from the adapter's title
		// and content. If found, override whatever PublishedAt says —
		// this is the single line that saves 04-11 from shipping Feb
		// press releases.
		if dt, ok := extractDateFromText(it.Title); ok {
			it.PublishedAt = dt
			fallbackHits++
		} else if dt, ok := extractDateFromText(it.Content); ok {
			it.PublishedAt = dt
			fallbackHits++
		}

		ts := it.PublishedAt
		if ts.IsZero() {
			ts = it.FetchedAt
		}
		if ts.IsZero() {
			droppedUnknown++
			continue
		}
		if ts.Before(cutoff) {
			droppedStale++
			continue
		}
		out = append(out, it)
	}
	if fallbackHits > 0 || droppedStale > 0 || droppedUnknown > 0 {
		fmt.Printf("[filter] title-date fallback: %d recovered, %d dropped as stale, %d dropped as undated\n",
			fallbackHits, droppedStale, droppedUnknown)
	}
	return out
}

// enrichLLMHeader merges fallback content into an LLM-generated HeaderCard
// to fill the gaps where the (legacy) LLM prompt produces fewer items than
// the new newspaper PIL layout expects. LLM fields stay authoritative; we
// only append/extend, never overwrite.
//
// Fields covered:
//   - SubHeadline: 如果 LLM 输出单行, 加 fallback 的第 2 条 (\n 拼接) 让 PIL
//     渲染出 L2 + L3 双层
//   - LeadParagraph: 如果 LLM 没输出或太短, 用 fallback 的
//   - KeyNumbers: 如果 LLM 输出 < 6, 用 fallback 的统计补到 6
//   - TopStories: 如果 LLM 输出 < 11, 用 fallback 的额外 stories 补到 11
//     (按 title 去重, LLM 的优先)
func enrichLLMHeader(h *infocard.HeaderCard, items []*store.IssueItem, summary string, issueNumber int, date string) {
	if h == nil {
		return
	}
	fb := buildFallbackHeaderCard(items, summary, issueNumber, date)

	// SubHeadline: LLM 单行 → 拼接 fb 的多行版本里的剩余行
	if !strings.Contains(h.SubHeadline, "\n") && fb.SubHeadline != "" {
		fbLines := strings.Split(fb.SubHeadline, "\n")
		// 跳过 fb 的第 1 行 (跟 LLM 的 L2 可能重复), 取 fb 的第 2 条作为 L3
		if len(fbLines) >= 2 {
			h.SubHeadline = h.SubHeadline + "\n" + fbLines[1]
		}
	}

	// LeadParagraph: LLM 没输出 → 用 fallback
	if strings.TrimSpace(h.LeadParagraph) == "" {
		h.LeadParagraph = fb.LeadParagraph
	}

	// KeyNumbers: 不够 6 个 → 用 fallback 的补
	if len(h.KeyNumbers) < 6 {
		seen := map[string]bool{}
		for _, kn := range h.KeyNumbers {
			seen[kn.Value] = true
		}
		for _, kn := range fb.KeyNumbers {
			if len(h.KeyNumbers) >= 6 {
				break
			}
			if seen[kn.Value] {
				continue
			}
			seen[kn.Value] = true
			h.KeyNumbers = append(h.KeyNumbers, kn)
		}
	}

	// TopStories: 不够 11 个 → 用 fallback 的补 (按 title 去重)
	if len(h.TopStories) < 11 {
		seen := map[string]bool{}
		for _, s := range h.TopStories {
			seen[strings.TrimSpace(s.Title)] = true
		}
		for _, s := range fb.TopStories {
			if len(h.TopStories) >= 14 {
				break
			}
			t := strings.TrimSpace(s.Title)
			if seen[t] {
				continue
			}
			seen[t] = true
			h.TopStories = append(h.TopStories, s)
		}
	}

	// Edition: 如果 LLM 没输出 → 用 fallback
	if strings.TrimSpace(h.Edition) == "" {
		h.Edition = fb.Edition
	}
	// IssueDate: 如果 LLM 没输出 → 用 fallback
	if strings.TrimSpace(h.IssueDate) == "" {
		h.IssueDate = fb.IssueDate
	}
	// FooterSlogan: 同上
	if strings.TrimSpace(h.FooterSlogan) == "" {
		h.FooterSlogan = fb.FooterSlogan
	}
}

// === infocard 本地兜底 (LLM 失败时用) ===
//
// 原理: infocard LLM 调用 (gpt-5.4 large JSON) 在 codex 上游一直 6 分钟
// 超时, 导致 PIL 拿不到 JSON, header.png 永远不更新, 大字报永远是旧的.
// 这个函数完全不调 LLM, 直接从已有的 issueItems + summary 用规则构造
// 一个合理的 HeaderCard, 喂给同一个 PIL 脚本生成新 PNG. 保证大字报永远
// 跟当天早报内容同步.
//
// 不追求跟 LLM 输出一样精炼 — 只追求"内容一定是今天的, 不会是历史残留".

var fallbackKeyNumRe = regexp.MustCompile(`(\d+%|\d+)`)

func buildFallbackHeaderCard(items []*store.IssueItem, summary string, issueNumber int, date string) *infocard.HeaderCard {
	// summary 拆行 + 去前缀 bullet
	stripBullet := func(s string) string {
		s = strings.TrimSpace(s)
		for _, p := range []string{"• ", "* ", "- ", "・", "· "} {
			s = strings.TrimPrefix(s, p)
		}
		return strings.TrimSpace(s)
	}
	truncRunes := func(s string, n int) string {
		s = strings.TrimSpace(s)
		rs := []rune(s)
		if len(rs) <= n {
			return s
		}
		return string(rs[:n-1]) + "…"
	}

	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(summary), "\n") {
		if l = stripBullet(l); l != "" {
			lines = append(lines, l)
		}
	}

	mainHeadline := "AI 日报今日要闻"
	if len(lines) > 0 {
		mainHeadline = lines[0]
	}
	// 按第一个逗号/句号截断, main_headline 是完整短句; 50 字硬限保险.
	for _, sep := range []string{"，", "。", "；", ",", ".", ";"} {
		if idx := strings.Index(mainHeadline, sep); idx > 0 {
			mainHeadline = mainHeadline[:idx]
			break
		}
	}
	mainHeadline = truncRunes(strings.TrimSpace(mainHeadline), 50)

	// L2/L3 次头条: 每行按第一个逗号/句号切是完整短句; 60 字硬限保险.
	var subLines []string
	for i := 1; i < len(lines) && len(subLines) < 3; i++ {
		line := lines[i]
		for _, sep := range []string{"，", "。", "；", ",", ".", ";"} {
			if idx := strings.Index(line, sep); idx > 0 {
				line = line[:idx]
				break
			}
		}
		subLines = append(subLines, truncRunes(strings.TrimSpace(line), 60))
	}
	subHeadline := strings.Join(subLines, "\n")
	if subHeadline == "" {
		subHeadline = "全网深度聚合 · 每日早读"
	}

	// Lead paragraph 加长: summary 全文 + items 的若干个 title 拼接, 总长
	// 280 字符让左大栏导语段填满 5-6 行.
	var leadParts []string
	leadParts = append(leadParts, lines...)
	for i, it := range items {
		if it == nil || i >= 4 {
			break
		}
		t := strings.TrimSpace(it.Title)
		t = strings.TrimLeft(t, "*")
		t = strings.TrimSpace(t)
		if t != "" {
			leadParts = append(leadParts, t)
		}
	}
	leadParagraph := truncRunes(strings.Join(leadParts, " · "), 280)

	// key_numbers: 从 summary 提取数字, 不够用统计补 (最多 5 个)
	var keyNums []infocard.KeyNum
	for _, m := range fallbackKeyNumRe.FindAllString(summary, -1) {
		if len(keyNums) >= 5 {
			break
		}
		keyNums = append(keyNums, infocard.KeyNum{Value: m, Label: "关键数字"})
	}
	sections := map[string]struct{}{}
	for _, it := range items {
		if it != nil {
			sections[it.Section] = struct{}{}
		}
	}
	stats := []infocard.KeyNum{
		{Value: fmt.Sprintf("%d", len(items)), Label: "今日条目"},
		{Value: fmt.Sprintf("%d", len(sections)), Label: "覆盖板块"},
		{Value: "21", Label: "信息源"},
		{Value: "24h", Label: "时间窗口"},
		{Value: "AI", Label: "领域"},
	}
	for _, st := range stats {
		if len(keyNums) >= 5 {
			break
		}
		keyNums = append(keyNums, st)
	}

	// top_stories: 最多 9 条 (PIL 模板 3x3), 每个 section 配额 (产品 3, 研究 2, 其他各 1)
	sectionLabels := map[string]string{
		"product_update": "产品",
		"research":       "研究",
		"industry":       "行业",
		"opensource":     "开源",
		"social":         "社会",
	}
	// quota 总和 = 14, 让 buildFallbackHeaderCard 输出 14 条 stories,
	// PIL mid (6) + bot (stories[6:14] 8 条) MORE STORIES 区能填满 8 条.
	sectionQuota := map[string]int{
		"product_update": 4,
		"research":       3,
		"industry":       3,
		"opensource":     2,
		"social":         2,
	}
	tagFor := func(section string) string {
		if t := sectionLabels[section]; t != "" {
			return t
		}
		if len(section) >= 2 {
			return strings.ToUpper(section[:2])
		}
		return strings.ToUpper(section)
	}
	// cleanTitleWithBody 把 title 跟 body 描述段拼接成完整的"一句话信息".
	// 关键: BodyMD 通常是 [image]\n\n[title heading]\n[描述段...] 结构,
	// title heading 跟 IssueItem.Title 完全一致 (LLM 同时输出). 我们用
	// string Index 找到 title 在 body 里的位置, trim 掉 title 之前+包括
	// title 的部分, 剩下的就是真正的描述段.
	cleanTitleWithBody := func(title, body string) string {
		t := strings.TrimSpace(title)
		t = strings.TrimLeft(t, "*")
		t = strings.TrimSpace(t)
		// 取大点的 body excerpt (200 字), 后面 trim title 后再缩到 80 字
		bodyExcerpt := buildPromptExcerpt(body, 200)
		if idx := strings.Index(bodyExcerpt, t); idx >= 0 {
			bodyExcerpt = strings.TrimSpace(bodyExcerpt[idx+len(t):])
			bodyExcerpt = strings.TrimLeft(bodyExcerpt, "·。.，,!?！？ \t")
			bodyExcerpt = strings.TrimSpace(bodyExcerpt)
		}
		// trim 完之后取前 80 字 (按 rune)
		runes := []rune(bodyExcerpt)
		if len(runes) > 80 {
			bodyExcerpt = string(runes[:80])
		}
		if bodyExcerpt != "" {
			t = t + " · " + bodyExcerpt
		}
		return truncRunes(t, 140)
	}
	cleanTitle := func(title string) string {
		t := strings.TrimSpace(title)
		t = strings.TrimLeft(t, "*")
		t = strings.TrimSpace(t)
		return truncRunes(t, 60)
	}
	var stories []infocard.TopStory
	sectionCount := map[string]int{}
	for _, it := range items {
		if len(stories) >= 14 {
			break
		}
		if it == nil {
			continue
		}
		cap := sectionQuota[it.Section]
		if cap == 0 {
			cap = 1
		}
		if sectionCount[it.Section] >= cap {
			continue
		}
		sectionCount[it.Section]++
		// 前 6 条 (mid TOP STORIES grid) 保持短 title 适配 cell width
		// 后 8 条 (bot MORE STORIES) 用 cleanTitleWithBody 拼接 body 摘要
		var titleStr string
		if len(stories) < 6 {
			titleStr = cleanTitle(it.Title)
		} else {
			titleStr = cleanTitleWithBody(it.Title, it.BodyMD)
		}
		stories = append(stories, infocard.TopStory{Title: titleStr, Tag: tagFor(it.Section)})
	}
	// 不够 11 条放宽 quota 再选一轮 (PIL bot zone 用 stories[6:11])
	if len(stories) < 11 {
		seen := map[string]struct{}{}
		for _, s := range stories {
			seen[s.Title] = struct{}{}
		}
		for _, it := range items {
			if len(stories) >= 14 {
				break
			}
			if it == nil {
				continue
			}
			t := cleanTitle(it.Title)
			if _, ok := seen[t]; ok {
				continue
			}
			seen[t] = struct{}{}
			stories = append(stories, infocard.TopStory{Title: t, Tag: tagFor(it.Section)})
		}
	}

	return &infocard.HeaderCard{
		IssueDate:     date,
		Edition:       fmt.Sprintf("v1.0.0 · 第 %d 期", issueNumber),
		MainHeadline:  mainHeadline,
		SubHeadline:   subHeadline,
		LeadParagraph: leadParagraph,
		KeyNumbers:    keyNums,
		TopStories:    stories,
		FooterSlogan:  "briefing-v3 · 全自动 AI 早报",
	}
}

// === 跨 run 去重 (sent_urls.txt 持久化) ===
//
// 设计原则: 用最小代码实现"同一天多次 run 不重复推送同一篇文章".
// 不动 sqlite schema, 不加 store interface 方法, 用一个简单的文件存
// URL set. fail-soft: 任何 IO 错误都只 log 不阻塞 pipeline.

const sentURLsPath = "data/sent_urls.txt"

// loadSentURLs 从持久化文件读取已推送 URL set. 文件不存在或 IO 错误
// 时返回空 set (首次运行 / 文件被清空 / 无权限读 都按"无历史"处理).
func loadSentURLs() map[string]bool {
	set := map[string]bool{}
	data, err := os.ReadFile(sentURLsPath)
	if err != nil {
		return set
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			set[line] = true
		}
	}
	return set
}

// appendSentURLs 把新一批 URL 追加到持久化文件. fail-soft: 任何错误
// 都只 print warning, 不返回 error (推送已经成功, dedup 是优化项).
func appendSentURLs(urls []string) {
	if len(urls) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(sentURLsPath), 0o755); err != nil {
		fmt.Printf("[WARN] dedup: mkdir %s: %v\n", filepath.Dir(sentURLsPath), err)
		return
	}
	f, err := os.OpenFile(sentURLsPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Printf("[WARN] dedup: open %s: %v\n", sentURLsPath, err)
		return
	}
	defer f.Close()
	for _, u := range urls {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		if _, err := f.WriteString(u + "\n"); err != nil {
			fmt.Printf("[WARN] dedup: write: %v\n", err)
			return
		}
	}
}

// dedupRawItemsBySent 返回 raw items 的子集, 排除 URL 已经在 sent set
// 中的条目. 如果 sent 为空 (首次运行) 直接返回原 slice.
func dedupRawItemsBySent(items []*store.RawItem, sent map[string]bool) []*store.RawItem {
	if len(sent) == 0 {
		return items
	}
	out := make([]*store.RawItem, 0, len(items))
	for _, it := range items {
		if it == nil {
			continue
		}
		if sent[it.URL] {
			continue
		}
		out = append(out, it)
	}
	return out
}

// --- v1.0.1: title-based dedup ---

const sentTitlesPath = "data/sent_titles.txt"

func loadSentTitles() map[string]bool {
	set := map[string]bool{}
	data, err := os.ReadFile(sentTitlesPath)
	if err != nil {
		return set
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			set[line] = true
		}
	}
	return set
}

func appendSentTitles(titles []string) {
	if len(titles) == 0 {
		return
	}
	if err := os.MkdirAll(filepath.Dir(sentTitlesPath), 0o755); err != nil {
		fmt.Printf("[WARN] title dedup: mkdir: %v\n", err)
		return
	}
	f, err := os.OpenFile(sentTitlesPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Printf("[WARN] title dedup: open: %v\n", err)
		return
	}
	defer f.Close()
	for _, t := range titles {
		t = strings.TrimSpace(t)
		if t != "" {
			_, _ = f.WriteString(t + "\n")
		}
	}
}

// titleKeywordRe extracts English proper nouns (4+ chars, capitalized)
// and Chinese phrases.
var titleKeywordRe = regexp.MustCompile(`[A-Z][a-zA-Z]{3,}|[\p{Han}]{3,}`)

func extractTitleKeywords(title string) []string {
	matches := titleKeywordRe.FindAllString(title, -1)
	// Deduplicate and lowercase for comparison.
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		key := strings.ToLower(m)
		if !seen[key] {
			seen[key] = true
			out = append(out, key)
		}
	}
	return out
}

// titleOverlap returns the Jaccard similarity between two keyword sets.
func titleOverlap(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	setA := map[string]bool{}
	for _, k := range a {
		setA[k] = true
	}
	inter := 0
	for _, k := range b {
		if setA[k] {
			inter++
		}
	}
	union := len(setA)
	for _, k := range b {
		if !setA[k] {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// dedupRawItemsByTitle removes items whose title keywords overlap >= 60%
// with already-sent titles. fail-soft: any error returns the original slice.
func dedupRawItemsByTitle(items []*store.RawItem, sentTitles map[string]bool) []*store.RawItem {
	if len(sentTitles) == 0 {
		return items
	}
	// Build keyword sets for all sent titles.
	var sentKWs [][]string
	for t := range sentTitles {
		kws := extractTitleKeywords(t)
		if len(kws) > 0 {
			sentKWs = append(sentKWs, kws)
		}
	}
	if len(sentKWs) == 0 {
		return items
	}

	out := make([]*store.RawItem, 0, len(items))
	for _, it := range items {
		if it == nil {
			continue
		}
		itemKWs := extractTitleKeywords(it.Title)
		if len(itemKWs) == 0 {
			out = append(out, it)
			continue
		}
		dup := false
		for _, skw := range sentKWs {
			if titleOverlap(itemKWs, skw) >= 0.6 {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, it)
		}
	}
	return out
}

// collectIssueItemSourceURLs 解出每个 IssueItem 的 SourceURLsJSON 字段
// 并扁平为单一 URL 列表, 用于 publish 后追加到 sent set. 解析失败的
// item 跳过 (不影响其他 item 的去重持久化).
func collectIssueItemSourceURLs(items []*store.IssueItem) []string {
	var urls []string
	for _, it := range items {
		if it == nil || strings.TrimSpace(it.SourceURLsJSON) == "" {
			continue
		}
		var us []string
		if err := json.Unmarshal([]byte(it.SourceURLsJSON), &us); err != nil {
			continue
		}
		for _, u := range us {
			u = strings.TrimSpace(u)
			if u != "" {
				urls = append(urls, u)
			}
		}
	}
	return urls
}

// buildFallbackSummary 在 LLM 上游临时不可用 (502/超时/etc) 时, 用本地
// item 标题拼一个 3 行的兜底 summary, 让 pipeline 不会因为 transient
// upstream error 整条退出. 取前 3 个 high-quality item 的标题作为 3 行.
// 如果 item 数 < 3 就只取实际数. 如果完全没 item, 返回一行通用兜底.
func buildFallbackSummary(items []*store.IssueItem) string {
	var lines []string
	for _, it := range items {
		if it == nil {
			continue
		}
		title := strings.TrimSpace(it.Title)
		if title == "" {
			continue
		}
		lines = append(lines, "• "+title)
		if len(lines) == 3 {
			break
		}
	}
	if len(lines) == 0 {
		return "今日 AI 早报已就绪 (LLM summary 临时不可用, 详见以下条目)."
	}
	return strings.Join(lines, "\n")
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

// buildGateFailAlert formats a Slack plain-text alert for the test
// channel. Soft warnings still publish to prod, so the wording must
// accurately say that the issue was delivered; hard failures still say
// prod was skipped.
func buildGateFailAlert(issue *store.Issue, r *gate.Report) string {
	var b strings.Builder
	if r.Warn {
		fmt.Fprintf(&b, "⚠️ briefing-v3 %s 质量待审,已同步正式频道\n", issue.IssueDate.Format("2006-01-02"))
	} else {
		fmt.Fprintf(&b, "🚨 briefing-v3 %s 质量不达标,正式频道已跳过\n", issue.IssueDate.Format("2006-01-02"))
	}
	fmt.Fprintf(&b, "• 条目数 %d | 非空 section %d | 洞察字数 %d\n",
		r.ItemCount, r.SectionCount, r.InsightChars)
	fmt.Fprintf(&b, "• 行业洞察 %d 条 | 启发 %d 条 | 独立源 %d 个\n",
		r.IndustryBullets, r.TakeawayBullets, r.SourceDomainCount)
	if len(r.FailedSections) > 0 {
		fmt.Fprintf(&b, "• 降级 section: %s\n", strings.Join(r.FailedSections, ","))
	}
	if len(r.Reasons) > 0 {
		b.WriteString("硬 fail 原因:\n")
		for _, reason := range r.Reasons {
			fmt.Fprintf(&b, "  - %s\n", reason)
		}
	}
	if len(r.Warnings) > 0 {
		b.WriteString("软 warn 原因:\n")
		for _, w := range r.Warnings {
			fmt.Fprintf(&b, "  - %s\n", w)
		}
	}
	return b.String()
}

func shouldPostGateAlert(r *gate.Report) bool {
	return r != nil && !r.Pass
}

func gateFailureBlocksRun(target string, r *gate.Report) bool {
	if r == nil || r.Pass {
		return false
	}
	// Soft warnings are allowed to ship to prod; only hard failures block.
	return !r.Warn
}

func buildProdReadinessAlert(issue *store.Issue, issues []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "⚠️ briefing-v3 %s 正式频道已跳过\n", issue.IssueDate.Format("2006-01-02"))
	b.WriteString("未满足正式频道发送前提:\n")
	for _, issue := range issues {
		fmt.Fprintf(&b, "  - %s\n", issue)
	}
	return b.String()
}

func prodPublishIssues(ctx context.Context, rendered *publish.RenderedIssue) []string {
	issues := make([]string, 0, 4)
	if rendered == nil || rendered.Issue == nil {
		return append(issues, "缺少日报对象")
	}
	if rendered.Insight == nil || strings.TrimSpace(rendered.Insight.IndustryMD) == "" {
		issues = append(issues, "缺少完整行业洞察")
	}
	if rendered.Insight == nil || strings.TrimSpace(rendered.Insight.OurMD) == "" {
		issues = append(issues, "缺少完整对我们的启发")
	}
	if strings.TrimSpace(rendered.Issue.Summary) == "" {
		issues = append(issues, "缺少完整今日摘要")
	}
	if msg := checkPublicReportURL(ctx, rendered.ReportURL); msg != "" {
		issues = append(issues, msg)
	}
	return issues
}

func checkPublicReportURL(ctx context.Context, rawURL string) string {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "完整版在线链接为空"
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "完整版在线链接格式无效"
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "完整版在线链接不是公网 HTTP(S) 地址"
	}

	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	status, err := urlProbe(checkCtx, http.MethodHead, rawURL)
	if err != nil || status >= 400 || status < 200 {
		status, err = urlProbe(checkCtx, http.MethodGet, rawURL)
		if err != nil {
			return "完整版在线链接当前不可访问"
		}
		if status < 200 || status >= 400 {
			return fmt.Sprintf("完整版在线链接返回异常状态 %d", status)
		}
	}
	return ""
}

var urlProbe = probeURL

func probeURL(ctx context.Context, method, rawURL string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, nil)
	if err != nil {
		return 0, err
	}
	hc := &http.Client{Timeout: 5 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	_, _ = io.CopyN(io.Discard, resp.Body, 1)
	return resp.StatusCode, nil
}

func gateFailureDetail(r *gate.Report) string {
	if r == nil {
		return "unknown gate state"
	}
	if r.Warn {
		if len(r.Warnings) == 0 {
			return "quality warning"
		}
		return strings.Join(r.Warnings, "; ")
	}
	if len(r.Reasons) == 0 {
		return "quality gate failed"
	}
	return strings.Join(r.Reasons, "; ")
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

// stripMediaFromIssueItems removes any ![alt](url) markdown images,
// [[VIDEO:url]] placeholders and common embed/hotlink fragments from
// every IssueItem.BodyMD. Used by the --no-images escape hatch so the
// text-only output never leaks image urls into Slack mrkdwn (which
// does not render markdown images, so they would show up as ugly
// literal text).
func stripMediaFromIssueItems(items []*store.IssueItem) {
	imgRe := regexp.MustCompile(`(?m)!\[[^\]]*\]\([^)]*\)[ \t]*\n?`)
	videoRe := regexp.MustCompile(`(?m)\[\[VIDEO:[^\]]*\]\][ \t]*\n?`)
	blankLines := regexp.MustCompile(`\n{3,}`)
	for _, it := range items {
		if it == nil {
			continue
		}
		body := it.BodyMD
		body = imgRe.ReplaceAllString(body, "")
		body = videoRe.ReplaceAllString(body, "")
		body = blankLines.ReplaceAllString(body, "\n\n")
		it.BodyMD = strings.TrimSpace(body)
	}
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
	// across that item's URL set, applying a cross-item de-duplication
	// step so the same hero image cannot be assigned to more than one
	// IssueItem (the 2026-04-10 run leaked 8× arxiv license badges and
	// 5× openai_logos_wall_money.png this way).
	//
	// Strategy: iterate all refs in order, and only accept an image
	// the first time we see that exact URL. Subsequent items keep
	// searching their other source URLs (each item has up to 3) for a
	// unique image. A first-seen tracker hit is also rejected here so
	// we never inject one.
	type collected struct {
		image string
		video string
		alt   string
	}
	byItem := make(map[int]*collected)
	seenImages := make(map[string]bool, len(allRefs))
	seenVideos := make(map[string]bool, len(allRefs))
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
			candidate := strings.TrimSpace(m.ImageURL)
			// Defence-in-depth: run the tracker filter again in case
			// parseHTML accepted a borderline URL. Keeping the check
			// here means enriching items is safe even if someone
			// bypasses the parser.
			if candidate != "" && !looksLikeMediaTracker(candidate) && !seenImages[candidate] {
				c.image = candidate
				seenImages[candidate] = true
				if strings.TrimSpace(m.AltText) != "" {
					c.alt = m.AltText
				}
			}
		}
		// v1.0.0 disables og:video capture entirely. Source og:video
		// fields in the wild are too often stale site intros / player
		// backgrounds with no relationship to the specific article, and
		// we have no reliable heuristic to classify which ones are
		// on-topic. Images are a safer default; callers can revisit
		// video later with a semantic filter.
		_ = seenVideos
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

	// -------------------------------------------------------------
	// FALLBACK: 对没抓到 og:image 的 item, 用 Pollinations 生成兜底图.
	// 用户原则: "优先从信息源链接下拿图,但如果没有允许生成,要确保是有解释
	// 意义的图". 关键是 prompt 必须精炼 - 不能像 v1.0.0 之前那样直接传
	// item.Title (那样 19/23 item 都被 Pollinations 兜底成不相关的"diagram
	// art", 用户判定为"空意义图"). 现在改成: title + body 首句摘要 (前 80
	// 字符), 让 diffusion 模型有更多语境去生成话题相关的图. 同时 fail-soft:
	// 单个 item 兜底失败也不能阻塞整条 pipeline.
	// -------------------------------------------------------------
	illusCount := 0
	for i, it := range items {
		if it == nil {
			continue
		}
		// Skip items that already got a real og:image above.
		if c, ok := byItem[i]; ok && c != nil && c.image != "" {
			continue
		}
		title := strings.TrimSpace(it.Title)
		if title == "" {
			continue
		}
		// Build a richer prompt: title + first ~80 chars of body excerpt.
		bodyExcerpt := buildPromptExcerpt(it.BodyMD, 80)
		prompt := title
		if bodyExcerpt != "" {
			prompt = title + ". " + bodyExcerpt
		}
		// Unique seed per item so no two items collide on the same
		// Pollinations cache key.
		seed := 10000 + (len(it.Section)*97 + it.Seq*31)
		url := illustration.BuildHotlinkURL(prompt, seed, 1200, 675)
		if url == "" {
			continue
		}
		alt := title
		for _, ch := range []string{"[", "]", "(", ")"} {
			alt = strings.ReplaceAll(alt, ch, " ")
		}
		alt = strings.TrimSpace(alt)

		// Append at the END of the body so the image sits below the
		// prose, mirroring mediaextract's existing append-behaviour.
		var b strings.Builder
		b.WriteString(strings.TrimRight(it.BodyMD, "\n"))
		b.WriteString("\n\n")
		fmt.Fprintf(&b, "![%s](%s)\n", alt, url)
		it.BodyMD = b.String()
		illusCount++
	}
	if illusCount > 0 {
		fmt.Printf("[media] fallback: %d items got a Pollinations infographic (rich prompt)\n", illusCount)
	}

	return enriched + illusCount
}

// buildPromptExcerpt 把 markdown body 剥成纯文本, 取**第二段以后**的前 n
// 字符 (按 rune 切). BodyMD 的结构通常是:
//
//	![card image](url)\n\n1. **title heading.**\n正文段落 1...\n\n正文段落 2...
//
// 第一段是 title heading (跟 IssueItem.Title 重复), 必须跳过, 否则拼接到
// title 后面会重复. 我们取第二段及之后作为真正的描述摘要.
var markdownImageRe = regexp.MustCompile(`!\[[^\]]*\]\([^)]*\)`)
var markdownLinkRe = regexp.MustCompile(`\[([^\]]*)\]\([^)]*\)`)
var listNumPrefixRe = regexp.MustCompile(`^\d+\.\s+`)

func buildPromptExcerpt(body string, n int) string {
	// 1. 删除 markdown image
	body = markdownImageRe.ReplaceAllString(body, "")
	// 2. 删除 markdown link 保留 text
	body = markdownLinkRe.ReplaceAllString(body, "$1")
	// 3. 按段落 split, 跳过第一段 (title heading)
	paragraphs := strings.Split(body, "\n\n")
	var nonEmpty []string
	for _, p := range paragraphs {
		p = strings.TrimSpace(p)
		if p != "" {
			nonEmpty = append(nonEmpty, p)
		}
	}
	var text string
	if len(nonEmpty) > 1 {
		text = strings.Join(nonEmpty[1:], " ")
	} else if len(nonEmpty) == 1 {
		text = nonEmpty[0]
	}
	// 4. 删除 emphasis / heading / 列表序号 等
	text = strings.ReplaceAll(text, "**", "")
	text = strings.ReplaceAll(text, "*", "")
	text = strings.ReplaceAll(text, "_", "")
	text = strings.ReplaceAll(text, "`", "")
	text = strings.ReplaceAll(text, "#", "")
	text = strings.ReplaceAll(text, "\n", " ")
	text = listNumPrefixRe.ReplaceAllString(text, "")
	for strings.Contains(text, "  ") {
		text = strings.ReplaceAll(text, "  ", " ")
	}
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) > n {
		text = string(runes[:n])
	}
	return text
}

// looksLikeMediaTracker is a thin wrapper that calls into the mediaextract
// package's heuristic. We keep a tiny duplicate here (rather than exporting
// looksLikeTracker from the package) so the defensive second-line filter
// in enrichItemsWithMedia stays close to the site-specific needles we may
// tweak on a production hotfix.
//
// At present it forwards the URL to a fresh mediaextract.Extractor, which
// does NOT do an HTTP fetch — we only call the filter path via a local
// helper. The mediaextract package recognises its own trackers the same
// way, so this behaves identically and is cheap.
func looksLikeMediaTracker(raw string) bool {
	lower := strings.ToLower(raw)
	// Mirror the most common hits; the canonical list lives inside the
	// mediaextract package and is also applied at parse time.
	blocklist := []string{
		"/icons/licenses", "licenses/by-", "by-nc-sa", "by-nc-nd",
		"arxiv.org/icons", "arxiv.org/favicons", "arxiv.org/static",
		"logos_wall", "_logos", "-logos", "logos.", "logos-", "/logos",
		"/logo", "logo.", "logo-", "logo_",
		"favicon", "apple-touch", "site-icon",
		"avatar", "gravatar", "profile-pic",
		"=s0-", "=w100", "=w150", "=w200", "=w300",
		"sprite", "social-", "share-button",
		"placeholder", "default-image", "no-image",
	}
	for _, needle := range blocklist {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
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
