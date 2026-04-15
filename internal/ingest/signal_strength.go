// signal_strength.go — v1.0.1 Phase 4.2.
//
// 目的: 给每个 RawItem 计算一个 "signal_strength" 分数, 表示有多少个不同
// 来源(按 URL host 区分)在报道同一件事. 输出用于 rank 阶段加权, 让跨源
// 共振的大新闻更容易进入 top 30.
//
// 算法:
//  1. 提取每条 item 的 "标题关键词" (英文 4+ 大写词 + 中文 3+ 字片段).
//  2. 两两比较 Jaccard 相似度, >= 0.5 视为同一件事.
//  3. 并查集合并, 每组内统计 distinct URL host 数 → SignalStrength.
//
// 复杂度 O(N²). N ≈ 100-300, 单次 run <50ms, 无需优化.
//
// 注意:
//   - 只依赖 item.Title + item.URL, 不读 Content (避免正文干扰).
//   - SignalStrength 是 per-item 写回内存字段, 不持久化.
//   - 短标题 (<2 关键词) 保守给 1, 不参与合并, 避免误伤.
package ingest

import (
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"briefing-v3/internal/store"
)

// signalTitleKeywordRe 抽取新闻标题里有区分度的"实体":
//   - [A-Z][A-Za-z]{3,}: 首字母大写的 4+ 字母词 (OpenAI / Anthropic / DeepMind)
//   - [A-Z]{2,}[-A-Za-z0-9]*: 全大写缩写 / 带版本号 (GPT-6 / GPT-4o / LLMs / API)
//   - [\p{Han}]{3,}: 中文 3+ 字连续片段
//
// 与 cmd/briefing/run.go:titleKeywordRe 的区别: 那里只用英文大写词 + 中文,
// 用于"与历史已推送去重" (阈值 0.6, 偏严); 这里是同一天多源共振合并 (阈值
// 0.5), 加上缩写识别让 GPT-6 / LLM / Claude-3.5 这类高频实体也能进桶.
var signalTitleKeywordRe = regexp.MustCompile(`[A-Z][A-Za-z]{3,}|[A-Z]{2,}[-A-Za-z0-9]*|[\p{Han}]{3,}`)

// extractSignalKeywords 返回 title 去重小写后的关键词 slice.
func extractSignalKeywords(title string) []string {
	matches := signalTitleKeywordRe.FindAllString(title, -1)
	seen := make(map[string]bool, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		k := strings.ToLower(m)
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// jaccardSimilarity 返回两个关键词集合的 Jaccard 系数, 0..1.
// 空集合返回 0 (避免 div-by-zero 也避免被误判为"完全一样").
func jaccardSimilarity(a, b []string) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	setA := make(map[string]bool, len(a))
	for _, k := range a {
		setA[k] = true
	}
	inter := 0
	union := len(setA)
	for _, k := range b {
		if setA[k] {
			inter++
		} else {
			union++
		}
	}
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// extractHost 从 URL 里抽 host. URL 为空或解析失败时返回 sourceKey, 避免
// 同一来源的两篇相似标题被误算成 2 个 distinct host.
func extractHost(rawURL string, sourceID int64) string {
	if rawURL == "" {
		return fallbackHost(sourceID)
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return fallbackHost(sourceID)
	}
	host := strings.ToLower(u.Host)
	// 去 www. 前缀, 防止 www.x.com 与 x.com 被算作两个 host.
	host = strings.TrimPrefix(host, "www.")
	return host
}

func fallbackHost(sourceID int64) string {
	// 保守: 给个唯一但可识别的 fallback, 防止多个空 URL 被合并成 host="".
	return "source#" + strconv.FormatInt(sourceID, 10)
}

// signalJaccardThreshold: 两条标题关键词 Jaccard >= 此阈值 → 同一件事.
// 0.5 是经验值 (titleOverlap 历史阈值 0.6 是"与历史已推送"去重用的,
// 更严; 这里是同一天内不同源之间合并, 放松到 0.5 能抓到大部分共振).
const signalJaccardThreshold = 0.5

// minKeywordsForGrouping: 少于此数量关键词的标题跳过合并 (signal=1),
// 避免短标题误合并 (例如两条都只有一个 "OpenAI" 关键词就被当一组).
const minKeywordsForGrouping = 2

// CalculateSignalStrength 遍历所有 items, 按标题相似度分组, 每组计算
// distinct host 数, 写回每个 item 的 SignalStrength 字段.
//
// 返回 distribution map: signal_strength → 有多少 items 是该值, 供调用
// 方日志用. 零 input / 全 nil 时返回空 map, 不报错.
func CalculateSignalStrength(items []*store.RawItem) map[int]int {
	dist := map[int]int{}
	if len(items) == 0 {
		return dist
	}

	// 预处理: 每个 item 抽关键词 + host. nil item 过滤掉但保留 index.
	n := len(items)
	kws := make([][]string, n)
	hosts := make([]string, n)
	validIdx := make([]int, 0, n)
	for i, it := range items {
		if it == nil {
			continue
		}
		kws[i] = extractSignalKeywords(it.Title)
		hosts[i] = extractHost(it.URL, it.SourceID)
		validIdx = append(validIdx, i)
	}

	// 并查集: parent[i] = i 初始.
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(x int) int {
		for parent[x] != x {
			parent[x] = parent[parent[x]]
			x = parent[x]
		}
		return x
	}
	union := func(x, y int) {
		px, py := find(x), find(y)
		if px != py {
			parent[px] = py
		}
	}

	// O(N²) 两两比较. 只合并关键词数 >= minKeywordsForGrouping 的条目.
	for ai := 0; ai < len(validIdx); ai++ {
		i := validIdx[ai]
		if len(kws[i]) < minKeywordsForGrouping {
			continue
		}
		for bi := ai + 1; bi < len(validIdx); bi++ {
			j := validIdx[bi]
			if len(kws[j]) < minKeywordsForGrouping {
				continue
			}
			if jaccardSimilarity(kws[i], kws[j]) >= signalJaccardThreshold {
				union(i, j)
			}
		}
	}

	// 每组统计 distinct host 数.
	groupHosts := make(map[int]map[string]bool)
	for _, i := range validIdx {
		root := find(i)
		if groupHosts[root] == nil {
			groupHosts[root] = map[string]bool{}
		}
		groupHosts[root][hosts[i]] = true
	}

	// 写回每个 item 的 SignalStrength.
	for _, i := range validIdx {
		it := items[i]
		root := find(i)
		ss := len(groupHosts[root])
		if ss < 1 {
			ss = 1
		}
		it.SignalStrength = ss
		dist[ss]++
	}

	return dist
}
