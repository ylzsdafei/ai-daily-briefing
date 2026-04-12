package main

import (
	"context"
	"testing"

	"briefing-v3/internal/gate"
	"briefing-v3/internal/publish"
	"briefing-v3/internal/store"
)

func TestGateFailureBlocksRun(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		target string
		report *gate.Report
		want   bool
	}{
		{
			name:   "auto_warn_is_non_fatal",
			target: "auto",
			report: &gate.Report{Pass: false, Warn: true, Warnings: []string{"section 覆盖不足"}},
			want:   false,
		},
		{
			name:   "prod_warn_is_non_fatal",
			target: "prod",
			report: &gate.Report{Pass: false, Warn: true, Warnings: []string{"section 覆盖不足"}},
			want:   false,
		},
		{
			name:   "auto_hard_fail_blocks",
			target: "auto",
			report: &gate.Report{Pass: false, Warn: false, Reasons: []string{"条目为零"}},
			want:   true,
		},
		{
			name:   "pass_never_blocks",
			target: "auto",
			report: &gate.Report{Pass: true},
			want:   false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := gateFailureBlocksRun(tc.target, tc.report); got != tc.want {
				t.Fatalf("gateFailureBlocksRun(%q) = %v, want %v", tc.target, got, tc.want)
			}
		})
	}
}

func TestGateFailureDetail(t *testing.T) {
	t.Parallel()

	if got := gateFailureDetail(&gate.Report{Pass: false, Warn: true, Warnings: []string{"条目不足", "section 覆盖不足"}}); got != "条目不足; section 覆盖不足" {
		t.Fatalf("unexpected warn detail: %q", got)
	}
	if got := gateFailureDetail(&gate.Report{Pass: false, Warn: false, Reasons: []string{"条目为零"}}); got != "条目为零" {
		t.Fatalf("unexpected fail detail: %q", got)
	}
}

func TestShouldPostGateAlert(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		report *gate.Report
		want   bool
	}{
		{
			name:   "nil_report",
			report: nil,
			want:   false,
		},
		{
			name:   "pass",
			report: &gate.Report{Pass: true},
			want:   false,
		},
		{
			name:   "warn",
			report: &gate.Report{Pass: false, Warn: true},
			want:   true,
		},
		{
			name:   "hard_fail",
			report: &gate.Report{Pass: false, Warn: false},
			want:   true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldPostGateAlert(tc.report); got != tc.want {
				t.Fatalf("shouldPostGateAlert() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProdPublishIssues(t *testing.T) {
	t.Run("complete_modules_and_public_link", func(t *testing.T) {
		origProbe := urlProbe
		urlProbe = func(ctx context.Context, method, rawURL string) (int, error) {
			return 200, nil
		}
		defer func() { urlProbe = origProbe }()

		rendered := &publish.RenderedIssue{
			Issue: &store.Issue{Summary: "1. 今日摘要"},
			Insight: &store.IssueInsight{
				IndustryMD: "1. 行业洞察",
				OurMD:      "1. 对我们的启发",
			},
			ReportURL: "https://briefing.example.com/2026/2026-04/2026-04-12/",
		}
		if issues := prodPublishIssues(context.Background(), rendered); len(issues) != 0 {
			t.Fatalf("prodPublishIssues() = %v, want empty", issues)
		}
	})

	t.Run("missing_modules_and_bad_link", func(t *testing.T) {
		rendered := &publish.RenderedIssue{
			Issue:     &store.Issue{},
			Insight:   &store.IssueInsight{},
			ReportURL: "file:///tmp/report.html",
		}
		issues := prodPublishIssues(context.Background(), rendered)
		if len(issues) != 4 {
			t.Fatalf("prodPublishIssues() len = %d, want 4; issues=%v", len(issues), issues)
		}
	})
}
