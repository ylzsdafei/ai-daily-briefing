// Package render — Hextra (Hugo) post writer.
//
// This file bridges briefing-v3's canonical markdown output (markdown.go)
// and the Hextra content tree at $HEXTRA_SITE_DIR. v1.0.0 enforces:
//
//   1. Three-level sidebar tree under content/cn/{year}/{yearMonth}/{date}.md
//      so Hextra renders a foldable 年 → 月 → 日 archive (otherwise the
//      sidebar runs off-screen after a few weeks).
//   2. Mandatory hero "新闻大字报" image at the top of every issue page.
//      Reads {workDir}/data/images/cards/{date}/header.png if present,
//      copies it into the Hextra static tree, and prepends a markdown
//      image reference. Missing header is logged but does not block the
//      write — the daily run is still published with text-only fallback.
//   3. Image scrub + relocate. The body produced by RenderMarkdown may
//      contain inline ![alt](url) references injected upstream by
//      infocard (item-N.png) and mediaextract (og:image hotlinks). We:
//        - VERIFY every external http(s) URL via HEAD: status 200 +
//          Content-Length in [5 KB, 50 MB]. mediaextract has its own
//          blacklist + multi-candidate scan to filter logos/banners,
//          and the HEAD check is a "minimum viable" guard so we never
//          publish a 404 / timeout / favicon-sized icon. Verified URLs
//          are kept verbatim so real article images, GitHub README
//          screenshots, arXiv figures, etc. survive end to end;
//        - COPY local PNGs from briefing-v3/data/images/cards/... into
//          {siteDir}/static/images/cards/{date}/ and rewrite the
//          markdown reference to a Hugo-friendly absolute path
//          /images/cards/{date}/<basename>;
//        - DELETE any reference whose target file does not exist (no
//          broken image icons leak through to the published page).
//
// All Hugo concerns live here. run.go / mediaextract / infocard / publish
// stay untouched.
package render

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"briefing-v3/internal/store"
)

// WriteHugoPost renders a full daily briefing and writes it as a
// Hextra-compatible markdown file under siteDir. The target path is
// deterministic and uses a three-level structure:
//
//	{siteDir}/content/cn/{YYYY}/{YYYY-MM}/{YYYY-MM-DD}.md
//
// Year and month directories are created on demand together with their
// _index.md scaffolds so the Hextra sidebar shows a collapsible
// 2026 → 2026-04 → 2026-04-11 AI资讯 tree.
//
// The function relies on RenderMarkdown to produce the canonical body,
// strips the markdown's leading "## AI资讯日报 YYYY/M/D" heading (Hextra
// generates its own H1 from frontmatter.title), prepends the hero
// header.png reference if available, then scrubs/relocates every
// remaining ![alt](url) image reference. WriteHugoPost finally writes
// the file with a hand-built Hextra frontmatter block and returns the
// absolute path of the written file.
func WriteHugoPost(
	siteDir string,
	issue *store.Issue,
	items []*store.IssueItem,
	insight *store.IssueInsight,
	sections []SectionMeta,
) (string, error) {
	if siteDir == "" {
		return "", fmt.Errorf("hugo: siteDir is empty")
	}
	if issue == nil {
		return "", fmt.Errorf("hugo: issue is nil")
	}

	body := RenderMarkdown(issue, items, insight, sections)

	// Strip the leading `## AI资讯日报 YYYY/M/D\n\n` line that markdown.go
	// emits as its own title — Hextra renders frontmatter.title as H1
	// so keeping the duplicate line would cause a double header.
	headLine := fmt.Sprintf("## AI资讯日报 %d/%d/%d",
		issue.IssueDate.Year(), int(issue.IssueDate.Month()), issue.IssueDate.Day())
	if strings.HasPrefix(body, headLine) {
		body = strings.TrimPrefix(body, headLine)
		body = strings.TrimLeft(body, "\n")
	}

	// --- date helpers ---------------------------------------------------
	date := issue.IssueDate
	year := date.Format("2006")
	yearMonth := date.Format("2006-01")
	dateStr := date.Format("2006-01-02")

	// --- hero header.png prepend ---------------------------------------
	// briefing-v3's infocard pass writes data/images/cards/{date}/header.png
	// relative to its working directory. We probe a few candidate roots so
	// the function works whether briefing is invoked from /root/briefing-v3
	// or from a systemd unit with a different WorkingDirectory.
	heroAbs := findExistingPath([]string{
		filepath.Join("data", "images", "cards", dateStr, "header.png"),
		filepath.Join("/root/briefing-v3/data/images/cards", dateStr, "header.png"),
	})
	if heroAbs != "" {
		// Copy into the Hextra static tree so Hugo picks it up at build.
		targetDir := filepath.Join(siteDir, "static", "images", "cards", dateStr)
		if err := os.MkdirAll(targetDir, 0o755); err == nil {
			targetPath := filepath.Join(targetDir, "header.png")
			if err := copyFile(heroAbs, targetPath); err == nil {
				heroLine := fmt.Sprintf("![新闻大字报 · %s](/images/cards/%s/header.png)\n\n",
					dateStr, dateStr)
				body = heroLine + body
			}
		}
	}

	// --- scrub & relocate every remaining ![alt](url) ------------------
	body = scrubAndRelocateImages(body, siteDir, dateStr)

	// --- frontmatter ----------------------------------------------------
	linkTitle := fmt.Sprintf("%02d-%02d AI资讯", int(date.Month()), date.Day())
	title := fmt.Sprintf("AI资讯日报 %d/%d/%d",
		date.Year(), int(date.Month()), date.Day())
	description := truncateDescription(issue.Summary, 150)

	var fm strings.Builder
	fm.WriteString("---\n")
	fmt.Fprintf(&fm, "linkTitle: %q\n", linkTitle)
	fmt.Fprintf(&fm, "title: %q\n", title)
	fmt.Fprintf(&fm, "weight: %d\n", date.Day())
	fm.WriteString("breadcrumbs: false\n")
	fm.WriteString("comments: false\n")
	fmt.Fprintf(&fm, "description: %q\n", description)
	fm.WriteString("---\n\n")

	full := fm.String() + body

	// --- filesystem: three-level tree -----------------------------------
	yearDir := filepath.Join(siteDir, "content", "cn", year)
	monthDir := filepath.Join(yearDir, yearMonth)
	if err := os.MkdirAll(monthDir, 0o755); err != nil {
		return "", fmt.Errorf("hugo: mkdir %s: %w", monthDir, err)
	}
	if err := ensureIndexFile(yearDir, year, date.Year()); err != nil {
		return "", fmt.Errorf("hugo: ensure year index: %w", err)
	}
	if err := ensureIndexFile(monthDir, yearMonth, int(date.Month())); err != nil {
		return "", fmt.Errorf("hugo: ensure month index: %w", err)
	}
	outPath := filepath.Join(monthDir, dateStr+".md")
	if err := os.WriteFile(outPath, []byte(full), 0o644); err != nil {
		return "", fmt.Errorf("hugo: write %s: %w", outPath, err)
	}
	return outPath, nil
}

// ensureIndexFile creates a minimal Hextra _index.md scaffold under dir
// if and only if one does not already exist. The title becomes the
// directory's display name in the sidebar; weight controls intra-level
// sort order. We never overwrite an existing _index.md, so a hand-tuned
// scaffold survives subsequent runs.
func ensureIndexFile(dir, title string, weight int) error {
	indexPath := filepath.Join(dir, "_index.md")
	if _, err := os.Stat(indexPath); err == nil {
		return nil // already exists, leave it alone
	}
	var b strings.Builder
	b.WriteString("---\n")
	fmt.Fprintf(&b, "title: %q\n", title)
	fmt.Fprintf(&b, "linkTitle: %q\n", title)
	fmt.Fprintf(&b, "weight: %d\n", weight)
	b.WriteString("breadcrumbs: false\n")
	b.WriteString("comments: false\n")
	b.WriteString("---\n")
	return os.WriteFile(indexPath, []byte(b.String()), 0o644)
}

// imageRefRe matches markdown image references: ![alt](url)
// alt can contain anything except ']'; url can contain anything except ')'.
var imageRefRe = regexp.MustCompile(`!\[([^\]]*)\]\(([^)]+)\)`)

// scrubAndRelocateImages walks the body and rewrites every ![alt](url)
// reference according to the v1.0.0 hard rules:
//
//   - http(s)://... external image  → DROP (avoid logo/banner noise)
//   - already /images/...           → KEEP (Hugo absolute path)
//   - local PNG that exists on disk → COPY into siteDir/static/images/cards/{date}/
//                                     and rewrite to /images/cards/{date}/<base>
//   - local PNG that does NOT exist → DROP (no broken image icons)
//
// All scrub decisions happen in a single pass so the body never carries
// a half-resolved reference into the published page.
func scrubAndRelocateImages(body, siteDir, dateStr string) string {
	targetDir := filepath.Join(siteDir, "static", "images", "cards", dateStr)
	mkdirOnce := false

	out := imageRefRe.ReplaceAllStringFunc(body, func(match string) string {
		m := imageRefRe.FindStringSubmatch(match)
		if len(m) < 3 {
			return ""
		}
		alt := strings.TrimSpace(m[1])
		url := strings.TrimSpace(m[2])
		if url == "" {
			return ""
		}

		// v1.0.0 修正: drop infocard L2 per-item PIL cards. The user
		// only wants ONE hero "大字报" at the top of the page (which
		// WriteHugoPost auto-prepends BEFORE this scrub runs, using a
		// /images/cards/{date}/header.png absolute path that will hit
		// the next case below). Every other item-*.png under cards/
		// is dropped — per-item cards cluttered the previous run.
		baseName := filepath.Base(url)
		if strings.HasPrefix(baseName, "item-") && strings.Contains(url, "cards/") {
			return ""
		}

		// Already a Hugo absolute path → keep as-is.
		if strings.HasPrefix(url, "/images/") {
			return match
		}

		// External http(s) URL: keep IF it passes a minimum-viable HEAD
		// probe (200 + Content-Length in [5 KB, 50 MB]). mediaextract
		// already filtered logos/banners via blacklist + multi-candidate
		// scan, so we trust the URL semantically and only verify it can
		// actually load. Anything that fails the HEAD (404, timeout,
		// favicon-sized icon, oversized binary) is dropped to avoid a
		// broken image icon leaking into the published page.
		if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
			if !verifyExternalImage(url) {
				return ""
			}
			return match
		}

		// Local file path. Try to resolve to an absolute path that
		// actually exists. infocard writes:
		//   ../data/images/cards/{date}/item-N.png   (relative)
		// or absolute paths under /root/briefing-v3/data/.
		candidates := []string{
			url,
			filepath.Join("/root/briefing-v3", url),
			filepath.Join("/root/briefing-v3", strings.TrimPrefix(url, "../")),
		}
		// Strip an arbitrary number of leading "../" prefixes.
		stripped := url
		for strings.HasPrefix(stripped, "../") {
			stripped = strings.TrimPrefix(stripped, "../")
			candidates = append(candidates, filepath.Join("/root/briefing-v3", stripped))
		}
		absPath := findExistingPath(candidates)
		if absPath == "" {
			// File missing → drop the reference.
			return ""
		}

		if !mkdirOnce {
			if err := os.MkdirAll(targetDir, 0o755); err != nil {
				return ""
			}
			mkdirOnce = true
		}

		base := filepath.Base(absPath)
		targetPath := filepath.Join(targetDir, base)
		if err := copyFile(absPath, targetPath); err != nil {
			return ""
		}

		newURL := fmt.Sprintf("/images/cards/%s/%s", dateStr, base)
		if alt == "" {
			return fmt.Sprintf("![](%s)", newURL)
		}
		return fmt.Sprintf("![%s](%s)", alt, newURL)
	})

	// Collapse three or more consecutive blank lines created by dropped
	// references so the markdown stays tidy.
	out = regexp.MustCompile(`\n{3,}`).ReplaceAllString(out, "\n\n")
	return out
}

// findExistingPath returns the first candidate path that exists on disk,
// or "" if none of them do. The check is a single os.Stat per entry.
func findExistingPath(candidates []string) string {
	for _, p := range candidates {
		if p == "" {
			continue
		}
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return ""
}

// copyFile streams src into dst with mode 0644. dst's parent directory
// must already exist; callers handle MkdirAll.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// bannedImageHosts is the explicit drop-list for image URL hosts that
// produce semantically empty or AI-hallucinated illustrations. We do not
// trust an upstream filter for these — any URL whose lower-cased form
// contains one of these substrings is unconditionally dropped before
// the HEAD probe even runs.
//
// Pollinations is the v1.0.0 motivating case: mediaextract falls back
// to image.pollinations.ai when the source page has no usable hero, and
// Pollinations responds with abstract AI art that has no real semantic
// relation to the news item (the user described it as "无关图片 +
// 大量重复"). It is also slow enough that the 5s HEAD probe usually
// times out, but that is incidental — we drop it explicitly so the
// behaviour does not depend on Pollinations latency.
var bannedImageHosts = []string{
	"image.pollinations.ai",
	"pollinations.ai",
}

// verifyExternalImage HEAD-probes an external image URL and returns
// true only when:
//   - the URL host is NOT on bannedImageHosts,
//   - the request completes within 5 s,
//   - the status code is 2xx,
//   - the response advertises a Content-Length in [5 KB, 50 MB] OR no
//     Content-Length at all (chunked encoding — we have no choice but
//     to trust the upstream filter in that case).
//
// This is the minimum-viable guard against banned hosts, 404, timeout,
// favicon-sized icons, and runaway binaries. The semantic "is this image
// actually related to the item" decision lives in mediaextract upstream,
// which already enforces a blacklist + multi-candidate scan;
// verifyExternalImage only catches the cases mediaextract cannot.
func verifyExternalImage(url string) bool {
	lowerURL := strings.ToLower(url)
	for _, banned := range bannedImageHosts {
		if strings.Contains(lowerURL, banned) {
			return false
		}
	}
	client := &http.Client{Timeout: 5 * time.Second}
	req, err := http.NewRequest(http.MethodHead, url, nil)
	if err != nil {
		return false
	}
	req.Header.Set("User-Agent", "briefing-v3/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false
	}
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		var size int64
		if _, err := fmt.Sscanf(cl, "%d", &size); err == nil {
			if size < 5*1024 || size > 50*1024*1024 {
				return false
			}
		}
	}
	return true
}

// truncateDescription keeps at most maxRunes runes of s, trimming
// whitespace at both ends and collapsing any embedded newlines to a
// single space so the result is safe to embed inside a double-quoted
// YAML scalar. Double quotes are escaped via the caller's %q.
func truncateDescription(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\r", " ")
	for strings.Contains(s, "  ") {
		s = strings.ReplaceAll(s, "  ", " ")
	}
	runes := []rune(s)
	if len(runes) > maxRunes {
		runes = runes[:maxRunes]
		s = string(runes)
	}
	return s
}
