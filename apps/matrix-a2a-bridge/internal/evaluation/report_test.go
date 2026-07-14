package evaluation

import (
	"strings"
	"testing"
	"time"
)

func fixtureResult(id string, latency int64, verdict Verdict) ScenarioResult {
	points := 1.0
	if verdict == VerdictFail {
		points = 0
	}
	var scorePoints *float64
	if verdict != VerdictUnscored {
		scorePoints = &points
	}
	return ScenarioResult{
		ScenarioID:          id,
		LatencyMilliseconds: latency,
		Usage: UsageDelta{
			Identity: ProviderIdentity{System: "openai", RequestModel: "m", Route: "llm"},
			TokenTypes: map[string]TokenDelta{
				"input":  {Requests: 1, Tokens: 10},
				"output": {Requests: 1, Tokens: 5},
			},
			LLMRequests: 1,
		},
		Score: Score{Verdict: verdict, Points: scorePoints},
	}
}

func TestBuildSummary(t *testing.T) {
	summary, err := BuildSummary([]ScenarioResult{
		fixtureResult("a", 10, VerdictPass),
		fixtureResult("b", 30, VerdictFail),
		fixtureResult("c", 20, VerdictUnscored),
	})
	if err != nil {
		t.Fatalf("BuildSummary: %v", err)
	}
	if summary.Scored != 2 || summary.Passed != 1 || summary.Failed != 1 || summary.Unscored != 1 {
		t.Fatalf("score summary = %#v", summary)
	}
	if summary.MeanLatencyMilliseconds != 20 || summary.P95LatencyMilliseconds != 30 {
		t.Fatalf("latency summary = %#v", summary)
	}
	if summary.Tokens["input"] != 30 || summary.Tokens["output"] != 15 || summary.LLMRequests != 3 {
		t.Fatalf("usage summary = %#v", summary)
	}
}

func TestMergeRunReplacesSameProfileAndRejectsCatalogDrift(t *testing.T) {
	report := NewReport(time.Unix(0, 0), "digest")
	run := ProfileRun{Profile: "vertex", RequestedModel: "m", Summary: RunSummary{}}
	if err := MergeRun(&report, run); err != nil {
		t.Fatalf("MergeRun: %v", err)
	}
	run.CompletedAt = time.Unix(1, 0)
	if err := MergeRun(&report, run); err != nil {
		t.Fatalf("MergeRun replace: %v", err)
	}
	if len(report.Runs) != 1 || !report.Runs[0].CompletedAt.Equal(run.CompletedAt) {
		t.Fatalf("runs = %#v", report.Runs)
	}
	report.Runs[0].Summary.EstimatedCost = &CostSummary{Currency: "EUR", CatalogVersion: "v1", EffectiveDate: "2026-07-01"}
	other := ProfileRun{
		Profile: "openai", RequestedModel: "n",
		Summary: RunSummary{EstimatedCost: &CostSummary{Currency: "USD", CatalogVersion: "v2", EffectiveDate: "2026-07-02"}},
	}
	if err := MergeRun(&report, other); err == nil {
		t.Fatal("MergeRun unexpectedly accepted incomparable pricing catalogs")
	}
}

func TestRenderMarkdownStatesMetricAndPricingBoundaries(t *testing.T) {
	report := NewReport(time.Unix(0, 0), "digest")
	report.Runs = []ProfileRun{{
		Profile: "vertex", RequestedModel: "m",
		Summary: RunSummary{
			Scored: 9, Passed: 8, Unscored: 1,
			ObservedProviderAndModel: ProviderIdentity{System: "gcp.vertex_ai", RequestModel: "m"},
			Tokens:                   map[string]uint64{"input": 10, "output": 5},
		},
	}}
	markdown := RenderMarkdown(report)
	for _, expected := range []string{"no Prometheus currency metric", "8/9 pass", "operator-supplied", "same series"} {
		if !strings.Contains(markdown, expected) {
			t.Errorf("markdown missing %q", expected)
		}
	}
}
