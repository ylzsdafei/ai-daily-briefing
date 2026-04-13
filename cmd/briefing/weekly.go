package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"briefing-v3/internal/config"
	"briefing-v3/internal/generate"
	"briefing-v3/internal/infocard"
	"briefing-v3/internal/render"
	"briefing-v3/internal/store"
)

// weeklyCommand generates a weekly analysis report for the ISO week
// containing the given date. It reads all daily issues from that week,
// calls the LLM for a structured weekly analysis, persists the result,
// writes a Hugo blog post, and optionally pushes to Slack.
func weeklyCommand(ctx context.Context, cfg *config.Config, date time.Time, gf *globalFlags) error {
	stage := func(msg string) {
		fmt.Printf("[%s] %s\n", time.Now().Format("15:04:05"), msg)
	}

	// --- resolve ISO week boundaries ---
	isoYear, isoWeek := date.ISOWeek()
	startDate := isoWeekStart(isoYear, isoWeek)
	endDate := startDate.AddDate(0, 0, 6) // Sunday
	stage(fmt.Sprintf("weekly: %d-W%02d (%s ~ %s)",
		isoYear, isoWeek,
		startDate.Format("2006-01-02"), endDate.Format("2006-01-02")))

	// --- open store ---
	s, err := store.New("data/briefing.db")
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}
	defer s.Close()
	if err := s.Migrate(ctx); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}

	// --- load daily issues for this week ---
	dailyIssues, err := s.ListDailyIssuesByDateRange(ctx, gf.domain, startDate, endDate)
	if err != nil {
		return fmt.Errorf("list daily issues: %w", err)
	}
	if len(dailyIssues) == 0 {
		return fmt.Errorf("weekly: no daily issues found for %d-W%02d (%s ~ %s)",
			isoYear, isoWeek,
			startDate.Format("2006-01-02"), endDate.Format("2006-01-02"))
	}
	stage(fmt.Sprintf("weekly: found %d daily issues", len(dailyIssues)))

	// --- assemble daily bundles ---
	var bundles []generate.DailyBundle
	var issueIDs []int64
	for _, di := range dailyIssues {
		items, err := s.ListIssueItems(ctx, di.ID)
		if err != nil {
			stage(fmt.Sprintf("[WARN] list items for issue %d: %v", di.ID, err))
			continue
		}
		insight, err := s.GetIssueInsight(ctx, di.ID)
		if err != nil {
			stage(fmt.Sprintf("[WARN] get insight for issue %d: %v", di.ID, err))
		}
		bundles = append(bundles, generate.DailyBundle{
			Issue:   di,
			Items:   items,
			Insight: insight,
		})
		issueIDs = append(issueIDs, di.ID)
	}
	if len(bundles) == 0 {
		return fmt.Errorf("weekly: no usable daily bundles")
	}

	// --- generate weekly analysis via LLM ---
	stage("weekly: calling LLM for weekly analysis")
	weeklyCfg := generate.WeeklyConfig{
		BaseURL:     cfg.LLM.BaseURL,
		APIKey:      cfg.LLM.APIKey,
		Model:       cfg.LLM.Model,
		Temperature: 0.4,
		Timeout:     180 * time.Second,
		MaxRetries:  3,
	}
	result, err := generate.GenerateWeekly(ctx, weeklyCfg, startDate, endDate, bundles)
	if err != nil {
		return fmt.Errorf("generate weekly: %w", err)
	}
	stage("weekly: LLM generation OK")

	// --- build full markdown ---
	title := fmt.Sprintf("第%d周 AI周报：%s", isoWeek, result.TitleKeywords)

	var fullMD strings.Builder
	fullMD.WriteString("## 本周聚焦\n\n")
	fullMD.WriteString(result.FocusMD)
	fullMD.WriteString("\n\n## 信号与噪音\n\n")
	fullMD.WriteString(result.SignalsMD)
	fullMD.WriteString("\n\n## 宏观趋势\n\n")
	fullMD.WriteString(result.TrendsMD)
	fullMD.WriteString("\n\n## 对我们的启发\n\n")
	fullMD.WriteString(result.TakeawaysMD)
	fullMD.WriteString("\n\n## 本周思考\n\n")
	fullMD.WriteString(result.PonderMD)

	// --- persist to DB ---
	issueIDsJSON, _ := json.Marshal(issueIDs)
	now := time.Now()
	weekly := &store.WeeklyIssue{
		DomainID:      gf.domain,
		Year:          isoYear,
		Week:          isoWeek,
		StartDate:     startDate,
		EndDate:       endDate,
		Title:         title,
		FocusMD:       result.FocusMD,
		SignalsMD:     result.SignalsMD,
		TrendsMD:      result.TrendsMD,
		TrendsDiagram: result.TrendsDiagram,
		TakeawaysMD:   result.TakeawaysMD,
		PonderMD:      result.PonderMD,
		FullMD:        fullMD.String(),
		DailyIssueIDs: string(issueIDsJSON),
		Status:        store.IssueStatusGenerated,
		GeneratedAt:   &now,
	}

	weeklyID, err := s.UpsertWeeklyIssue(ctx, weekly)
	if err != nil {
		return fmt.Errorf("upsert weekly: %w", err)
	}
	stage(fmt.Sprintf("weekly: persisted to DB (id=%d)", weeklyID))

	// --- dry-run: print and exit ---
	if gf.dryRun {
		stage("dry-run: skipping hugo write and publish")
		fmt.Println("\n================ WEEKLY MARKDOWN ================")
		fmt.Println(fullMD.String())
		fmt.Println("=================================================")
		return nil
	}

	// --- generate weekly header card (大字报) ---
	if !gf.noImages {
		weeklyHeader := buildWeeklyHeaderCard(weekly, result)
		weeklyDateStr := fmt.Sprintf("%d-W%02d", isoYear, isoWeek)
		headerDir := fmt.Sprintf("data/images/cards/%s", weeklyDateStr)
		if err := os.MkdirAll(headerDir, 0o755); err == nil {
			headerPath := headerDir + "/header.png"
			if err := renderInfoCardPNG(ctx, "header", weeklyHeader, headerPath); err != nil {
				fmt.Printf("[WARN] weekly headercard: %v\n", err)
			} else {
				stage(fmt.Sprintf("weekly: header card → %s", headerPath))
			}
		}
	}

	// --- write Hugo post ---
	hextraDir := os.Getenv("HEXTRA_SITE_DIR")
	if hextraDir != "" {
		hugoPath, hugoErr := render.WriteWeeklyPost(hextraDir, weekly, dailyIssues)
		if hugoErr != nil {
			fmt.Printf("[WARN] weekly hugo write failed: %v (continuing)\n", hugoErr)
		} else {
			stage(fmt.Sprintf("weekly hugo: wrote %s", hugoPath))
		}

		// Hugo build if HUGO_BIN is set.
		if hugoBin := os.Getenv("HUGO_BIN"); hugoBin != "" {
			stage("weekly: running hugo build")
			if err := hugoBuildf(ctx, hugoBin, hextraDir); err != nil {
				fmt.Printf("[WARN] hugo build: %v\n", err)
			} else {
				stage("weekly: hugo build OK")
			}
		}
	}

	// --- Slack publish ---
	targetWantsProd := gf.target == "auto" || gf.target == "prod"
	// Build weekly page URL from BRIEFING_REPORT_URL_BASE.
	weeklyPageURL := ""
	if base := os.Getenv("BRIEFING_REPORT_URL_BASE"); base != "" {
		if idx := strings.Index(base, "{{"); idx > 0 {
			siteRoot := strings.TrimRight(base[:idx], "/")
			weeklyPageURL = fmt.Sprintf("%s/blog/weekly/%d-w%02d/", siteRoot, isoYear, isoWeek)
		}
	}
	slackBlocks := buildWeeklySlackBlocks(weekly, dailyIssues, weeklyPageURL)
	slackBody, _ := json.Marshal(slackBlocks)

	stage("weekly: posting to Slack test channel")
	testDelivery := postSlackPayload(ctx, store.ChannelSlackTest, cfg.Slack.TestWebhook, slackBody, 0)
	if testDelivery.Status != store.DeliveryStatusSent {
		fmt.Printf("[WARN] weekly slack test: %s\n", testDelivery.ResponseJSON)
	}

	if targetWantsProd {
		stage("weekly: posting to Slack prod channel")
		prodDelivery := postSlackPayload(ctx, store.ChannelSlackProd, cfg.Slack.ProdWebhook, slackBody, 0)
		if prodDelivery.Status != store.DeliveryStatusSent {
			fmt.Printf("[WARN] weekly slack prod: %s\n", prodDelivery.ResponseJSON)
		} else {
			stage("weekly: slack prod OK")
		}
	}

	stage("weekly: done")
	return nil
}

// isoWeekStart returns the Monday of the given ISO year/week.
func isoWeekStart(isoYear, isoWeek int) time.Time {
	// Jan 4 is always in ISO week 1.
	jan4 := time.Date(isoYear, 1, 4, 0, 0, 0, 0, time.UTC)
	// Weekday offset: Monday=0 ... Sunday=6.
	offset := int(jan4.Weekday()+6) % 7 // days since Monday
	week1Monday := jan4.AddDate(0, 0, -offset)
	return week1Monday.AddDate(0, 0, (isoWeek-1)*7)
}

// buildWeeklySlackBlocks creates a Slack blocks payload for the weekly report,
// matching the daily report's button style (primary blue button).
func buildWeeklySlackBlocks(w *store.WeeklyIssue, dailyIssues []*store.Issue, weeklyPageURL string) map[string]any {
	var blocks []map[string]any

	// Header
	blocks = append(blocks, map[string]any{
		"type": "header",
		"text": map[string]any{"type": "plain_text", "text": w.Title, "emoji": true},
	})

	// Date range context
	blocks = append(blocks, map[string]any{
		"type": "context",
		"elements": []map[string]any{
			{"type": "mrkdwn", "text": fmt.Sprintf("📅 %s ~ %s | %d 期日报汇总",
				w.StartDate.Format("2006-01-02"),
				w.EndDate.Format("2006-01-02"),
				len(dailyIssues))},
		},
	})

	blocks = append(blocks, map[string]any{"type": "divider"})

	// 本周聚焦 (truncated)
	if focus := strings.TrimSpace(w.FocusMD); focus != "" {
		runes := []rune(mdToSlack(focus))
		if len(runes) > 600 {
			focus = string(runes[:600]) + "..."
		} else {
			focus = string(runes)
		}
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": "*🎯 本周聚焦*\n" + focus},
		})
	}

	blocks = append(blocks, map[string]any{"type": "divider"})

	// 对我们的启发
	if takeaways := strings.TrimSpace(w.TakeawaysMD); takeaways != "" {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": "*💡 对我们的启发*\n" + mdToSlack(takeaways)},
		})
	}

	// 本周思考
	if ponder := strings.TrimSpace(w.PonderMD); ponder != "" {
		blocks = append(blocks, map[string]any{
			"type": "section",
			"text": map[string]any{"type": "mrkdwn", "text": "*🤔 本周思考*\n" + mdToSlack(ponder)},
		})
	}

	// Button — same style as daily report
	if weeklyPageURL != "" {
		blocks = append(blocks, map[string]any{
			"type": "actions",
			"elements": []map[string]any{
				{
					"type": "button",
					"text": map[string]any{
						"type":  "plain_text",
						"text":  "📖 查看完整周报",
						"emoji": true,
					},
					"url":   weeklyPageURL,
					"style": "primary",
				},
			},
		})
	}

	// Footer
	blocks = append(blocks, map[string]any{
		"type": "context",
		"elements": []map[string]any{
			{"type": "mrkdwn", "text": "briefing-v3 自动生成 · AI 周报"},
		},
	})

	return map[string]any{"blocks": blocks}
}

// mdToSlack converts standard markdown to Slack mrkdwn format:
//   - ### Header → *Header*
//   - **bold** → *bold*
//   - [text](url) → <url|text>
//   - Strip remaining # prefixes
func mdToSlack(s string) string {
	// Headers: ### text → *text*
	s = regexp.MustCompile(`(?m)^#{1,6}\s+(.+)$`).ReplaceAllString(s, "*$1*")
	// Bold: **text** → *text*
	s = regexp.MustCompile(`\*\*([^*]+)\*\*`).ReplaceAllString(s, "*$1*")
	// Links: [text](url) → <url|text>
	s = regexp.MustCompile(`\[([^\]]+)\]\(([^)]+)\)`).ReplaceAllString(s, "<$2|$1>")
	return s
}

// buildWeeklyHeaderCard constructs a HeaderCard for the weekly report's
// 大字报 PNG. Reuses the same PIL template as the daily headercard.
func buildWeeklyHeaderCard(w *store.WeeklyIssue, result *generate.WeeklyResult) *infocard.HeaderCard {
	truncRunes := func(s string, n int) string {
		rs := []rune(strings.TrimSpace(s))
		if len(rs) <= n {
			return string(rs)
		}
		return string(rs[:n-1]) + "…"
	}

	// Main headline from title keywords.
	mainHeadline := truncRunes(w.Title, 40)

	// Sub headlines from the first line of each focus topic (### titles).
	var subLines []string
	for _, line := range strings.Split(result.FocusMD, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "### ") {
			// Strip "### 1. " prefix → keep the topic title.
			topic := strings.TrimPrefix(line, "### ")
			// Remove leading number.
			if idx := strings.Index(topic, " "); idx > 0 && idx < 5 {
				topic = strings.TrimSpace(topic[idx:])
			}
			subLines = append(subLines, truncRunes(topic, 50))
		}
		if len(subLines) >= 3 {
			break
		}
	}
	subHeadline := strings.Join(subLines, "\n")

	// Lead paragraph from trends summary.
	lead := truncRunes(strings.ReplaceAll(strings.TrimSpace(result.TrendsMD), "\n", " "), 160)

	// Key numbers: week number, daily count, keyword count.
	keyNums := []infocard.KeyNum{
		{Value: fmt.Sprintf("W%d", w.Week), Label: "本周期号"},
		{Value: fmt.Sprintf("%s~%s", w.StartDate.Format("01/02"), w.EndDate.Format("01/02")), Label: "覆盖日期"},
	}

	// Top stories from focus topics.
	var stories []infocard.TopStory
	for _, sl := range subLines {
		stories = append(stories, infocard.TopStory{Title: sl, Tag: "聚焦"})
	}

	return &infocard.HeaderCard{
		IssueDate:     fmt.Sprintf("%d-W%02d", w.Year, w.Week),
		Edition:       fmt.Sprintf("AI 周报 · 第 %d 周", w.Week),
		MainHeadline:  mainHeadline,
		SubHeadline:   subHeadline,
		LeadParagraph: lead,
		KeyNumbers:    keyNums,
		TopStories:    stories,
		FooterSlogan:  "每周一更 · 趋势尽览",
	}
}

// hugoBuildf runs hugo --source {siteDir} with a timeout.
func hugoBuildf(ctx context.Context, hugoBin, siteDir string) error {
	subCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	cmd := exec.CommandContext(subCtx, hugoBin, "--source", siteDir, "--gc", "--minify")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}
