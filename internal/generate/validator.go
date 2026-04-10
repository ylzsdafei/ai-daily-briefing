package generate

import (
	"regexp"
	"strings"
)

// BannedPattern describes a regex pattern that, if matched in insight output,
// causes validation to fail. Ported from slack-notify.js INSIGHT_BANNED_PATTERNS.
type BannedPattern struct {
	Pattern *regexp.Regexp
	Reason  string
}

// bannedPatterns mirrors INSIGHT_BANNED_PATTERNS in slack-notify.js (rows 18-34).
// These reject operational / ops-leakage language from the LLM output.
var bannedPatterns = []BannedPattern{
	{Pattern: regexp.MustCompile(`(?i)webhook`), Reason: "包含发布通道细节"},
	{Pattern: regexp.MustCompile(`(?i)\bcron\b`), Reason: "包含调度实现细节"},
	{Pattern: regexp.MustCompile(`(?i)\bschedule\b`), Reason: "包含调度实现细节"},
	{Pattern: regexp.MustCompile(`(?i)GitHub Actions`), Reason: "包含运维平台细节"},
	{Pattern: regexp.MustCompile(`缓存`), Reason: "包含缓存或排障细节"},
	{Pattern: regexp.MustCompile(`轮询`), Reason: "包含调度策略细节"},
	{Pattern: regexp.MustCompile(`幂等`), Reason: "包含工程实现细节"},
	{Pattern: regexp.MustCompile(`北京时间`), Reason: "包含排障时间戳"},
	{Pattern: regexp.MustCompile(`\b\d{1,2}:\d{2}(?::\d{2})?\b`), Reason: "包含具体运行时间"},
	{Pattern: regexp.MustCompile(`推送链路`), Reason: "包含内部投递链路表述"},
	{Pattern: regexp.MustCompile(`测试频道`), Reason: "包含内部频道信息"},
	{Pattern: regexp.MustCompile(`正式频道`), Reason: "包含内部频道信息"},
	{Pattern: regexp.MustCompile(`告警`), Reason: "包含内部监控表述"},
	{Pattern: regexp.MustCompile(`补发`), Reason: "包含内部操作表述"},
	{Pattern: regexp.MustCompile(`本地设备`), Reason: "包含排障上下文"},
}

// ValidationResult describes the outcome of validating an insight string.
type ValidationResult struct {
	OK          bool
	Reasons     []string
	IndustryRaw string
	OurRaw      string
}

// splitInsightRegex splits on the "💭 对我们的启发" header, optionally eating leading
// '#' markers. Mirrors the JS split regex.
var splitInsightRegex = regexp.MustCompile(`(?:#{1,6}\s*)?💭\s*对我们的启发[^）]*[）)]?\s*`)

// industryHeaderRegex strips the "📊 行业洞察" header from the industry chunk.
var industryHeaderRegex = regexp.MustCompile(`(?:#{1,6}\s*)?📊\s*行业洞察[^）]*[）)]?\s*\n*`)

// leadingHeaderRegex strips leading markdown header symbols at the start of any line.
var leadingHeaderRegex = regexp.MustCompile(`(?m)^#{1,6}\s+`)

// trailingHeaderRegex strips any trailing '##' markers left dangling at the end.
var trailingHeaderRegex = regexp.MustCompile(`\s*#{1,6}\s*$`)

// numberedItemRegex counts "1.", "2." style lines. Mirrors JS countNumberedItems.
var numberedItemRegex = regexp.MustCompile(`(?m)^\d+\.`)

// ParseInsightSections splits the LLM output into its "industry insight" and
// "for us" halves. It mirrors parseInsightSections() in slack-notify.js
// (rows 82-98), including the '##' cleanup that was added recently.
func ParseInsightSections(raw string) (industry, our string) {
	if raw == "" {
		return "", ""
	}

	parts := splitInsightRegex.Split(raw, 2)

	var industryRaw, ourRaw string
	if len(parts) > 0 {
		industryRaw = parts[0]
	}
	if len(parts) > 1 {
		ourRaw = parts[1]
	}

	// Clean industry side: strip leading "📊 行业洞察..." header,
	// strip leading '## ' per-line, strip trailing '##'.
	industryRaw = industryHeaderRegex.ReplaceAllString(industryRaw, "")
	industryRaw = leadingHeaderRegex.ReplaceAllString(industryRaw, "")
	industryRaw = trailingHeaderRegex.ReplaceAllString(industryRaw, "")
	industryRaw = strings.TrimSpace(industryRaw)

	// Clean our side: same header stripping (but no "📊 行业洞察" prefix).
	ourRaw = leadingHeaderRegex.ReplaceAllString(ourRaw, "")
	ourRaw = trailingHeaderRegex.ReplaceAllString(ourRaw, "")
	ourRaw = strings.TrimSpace(ourRaw)

	return industryRaw, ourRaw
}

// countNumberedItems mirrors the JS countNumberedItems: count lines beginning
// with "\d+.".
func countNumberedItems(text string) int {
	return len(numberedItemRegex.FindAllString(text, -1))
}

// ValidateInsight checks that the LLM output contains both sections, that
// each section has the expected bullet count, and that no banned pattern
// appears. Mirrors validateInsightOutput() in slack-notify.js (rows 104-132).
func ValidateInsight(raw string) ValidationResult {
	industryRaw, ourRaw := ParseInsightSections(raw)

	var reasons []string

	if industryRaw == "" || ourRaw == "" {
		reasons = append(reasons, "缺少\"行业洞察\"或\"对我们的启发\"模块")
	}

	industryCount := countNumberedItems(industryRaw)
	ourCount := countNumberedItems(ourRaw)

	if industryCount < 3 || industryCount > 4 {
		reasons = append(reasons,
			"行业洞察条数异常（当前 "+itoa(industryCount)+" 条）")
	}
	if ourCount < 2 || ourCount > 3 {
		reasons = append(reasons,
			"对我们的启发条数异常（当前 "+itoa(ourCount)+" 条）")
	}

	for _, bp := range bannedPatterns {
		if bp.Pattern.MatchString(raw) {
			reasons = append(reasons, bp.Reason)
		}
	}

	return ValidationResult{
		OK:          len(reasons) == 0,
		Reasons:     reasons,
		IndustryRaw: industryRaw,
		OurRaw:      ourRaw,
	}
}

// itoa is a tiny int->string helper to avoid pulling strconv just for this.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
