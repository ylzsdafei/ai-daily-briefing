// rank_test.go — unit tests for v1.0.1 Phase 4.1 priority weighting.
//
// Design: priorityWeight is a pure function so we test it directly.
// The full Rank() flow (LLM call + sort + quota) is integration-tested
// via briefing run --dry-run; here we focus on the new weighting logic.
//
// Run with: go test ./internal/rank/ -run PriorityWeight -v

package rank

import (
	"sort"
	"testing"

	"briefing-v3/internal/store"
)

func TestPriorityWeight(t *testing.T) {
	tests := []struct {
		name       string
		priorities map[int64]int
		sourceID   int64
		want       float64
	}{
		{"nil map → neutral 1.0", nil, 42, 1.0},
		{"empty map → neutral 1.0", map[int64]int{}, 42, 1.0},
		{"sourceID missing from populated map → neutral 1.0", map[int64]int{1: 10}, 42, 1.0},
		{"priority 10 (顶级权威源)", map[int64]int{42: 10}, 42, 1.5},
		{"priority 9", map[int64]int{42: 9}, 42, 1.4},
		{"priority 7", map[int64]int{42: 7}, 42, 1.2},
		{"priority 5 (中性基线)", map[int64]int{42: 5}, 42, 1.0},
		{"priority 3", map[int64]int{42: 3}, 42, 0.8},
		{"priority 0 (未设, 降权)", map[int64]int{42: 0}, 42, 0.5},
		{"priority 15 (越界, clamp 到 10)", map[int64]int{42: 15}, 42, 1.5},
		{"priority -5 (负数, clamp 到 0)", map[int64]int{42: -5}, 42, 0.5},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := priorityWeight(tc.priorities, tc.sourceID)
			if got != tc.want {
				t.Errorf("priorityWeight(%v, %d) = %v, want %v",
					tc.priorities, tc.sourceID, got, tc.want)
			}
		})
	}
}

// TestRankedItemSortOrder 验证按 WeightedScore 排序的结果符合直觉.
// 不依赖 LLM; 我们直接构造 RankedItem 然后 sort.
func TestRankedItemSortOrder(t *testing.T) {
	// 场景: 两个 item 原始 LLM 分都是 7, 但来自不同 priority 的源.
	// priority 10 的应该排在 priority 5 前.
	//
	// 另一个边界: 高 priority 低分数 vs 低 priority 高分数. 看 weighted 结果.
	//   priority 10, Score 6  → Weighted 9.0
	//   priority 5,  Score 8  → Weighted 8.0
	//   → priority 10 仍然胜 (低分官方 > 高分无名)

	items := []*RankedItem{
		{Item: &store.RawItem{ID: 1, SourceID: 100, Title: "weak but authoritative"}, Score: 6, WeightedScore: 6 * 1.5}, // priority 10
		{Item: &store.RawItem{ID: 2, SourceID: 200, Title: "strong but unknown"}, Score: 8, WeightedScore: 8 * 1.0},     // priority 5
		{Item: &store.RawItem{ID: 3, SourceID: 300, Title: "mediocre random blog"}, Score: 7, WeightedScore: 7 * 0.6},   // priority 1
		{Item: &store.RawItem{ID: 4, SourceID: 100, Title: "top authoritative"}, Score: 9, WeightedScore: 9 * 1.5},      // priority 10
		{Item: &store.RawItem{ID: 5, SourceID: 200, Title: "average mainstream"}, Score: 7, WeightedScore: 7 * 1.0},     // priority 5
	}

	// Sort by same logic as rank.go Rank().
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].WeightedScore != items[j].WeightedScore {
			return items[i].WeightedScore > items[j].WeightedScore
		}
		if items[i].Score != items[j].Score {
			return items[i].Score > items[j].Score
		}
		return items[i].Item.ID < items[j].Item.ID
	})

	// Expected: id 4 (9*1.5=13.5), 1 (6*1.5=9.0), 2 (8*1.0=8.0), 5 (7*1.0=7.0), 3 (7*0.6=4.2)
	expectedOrder := []int64{4, 1, 2, 5, 3}
	for i, want := range expectedOrder {
		if items[i].Item.ID != want {
			t.Errorf("position %d: got id %d (%q, weighted %.2f), want id %d",
				i, items[i].Item.ID, items[i].Item.Title, items[i].WeightedScore, want)
		}
	}
}

func TestRankedItem_TieBreaker(t *testing.T) {
	// 相同 WeightedScore → 按 Score desc → 按 ID asc
	items := []*RankedItem{
		{Item: &store.RawItem{ID: 3, SourceID: 1}, Score: 7, WeightedScore: 7.0}, // same weighted
		{Item: &store.RawItem{ID: 1, SourceID: 2}, Score: 7, WeightedScore: 7.0}, // same weighted, lower ID
		{Item: &store.RawItem{ID: 2, SourceID: 3}, Score: 8, WeightedScore: 7.0}, // same weighted, higher raw Score
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].WeightedScore != items[j].WeightedScore {
			return items[i].WeightedScore > items[j].WeightedScore
		}
		if items[i].Score != items[j].Score {
			return items[i].Score > items[j].Score
		}
		return items[i].Item.ID < items[j].Item.ID
	})
	// Expected: id 2 (weighted 7, raw 8) > id 1 (weighted 7, raw 7, lower ID) > id 3 (weighted 7, raw 7, higher ID)
	expectedOrder := []int64{2, 1, 3}
	for i, want := range expectedOrder {
		if items[i].Item.ID != want {
			t.Errorf("position %d: got id %d, want id %d (tie-breaker wrong)",
				i, items[i].Item.ID, want)
		}
	}
}
