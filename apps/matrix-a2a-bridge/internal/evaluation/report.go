package evaluation

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	// ReportSchemaVersion identifies the machine-readable report contract.
	ReportSchemaVersion = "fgentic.eval.report.v1"
	// AgentgatewayVersion pins the source-inspected metrics implementation.
	AgentgatewayVersion = "v1.3.1"
)

// ScenarioResult captures one answer, score, latency, usage delta, and optional estimate.
type ScenarioResult struct {
	ScenarioID          string        `json:"scenario_id"`
	Agent               Agent         `json:"agent"`
	Prompt              string        `json:"prompt"`
	Rubric              Rubric        `json:"rubric"`
	Answer              string        `json:"answer"`
	LatencyMilliseconds int64         `json:"latency_milliseconds"`
	Usage               UsageDelta    `json:"usage"`
	EstimatedCost       *CostEstimate `json:"estimated_cost,omitempty"`
	Score               Score         `json:"score"`
	// JudgeScores holds the content-free sovereign LLM-as-judge score pair when the judge lane scored
	// this scenario; nil when the deterministic rubric applied or the judge lane was blocked (#355).
	JudgeScores *JudgeScores `json:"judge_scores,omitempty"`
	// Faithfulness holds the content-free citation-faithfulness result when the scenario carried a
	// corpus-cited answer contract and the sovereign judge lane was approved; nil otherwise. It surfaces
	// only verdicts and claim/chunk IDs alongside groundedness — never claim prose or chunk text (D7,
	// #358).
	Faithfulness *FaithfulnessResult `json:"faithfulness,omitempty"`
}

// CostSummary aggregates estimates made with one catalog identity.
type CostSummary struct {
	Currency       string  `json:"currency"`
	Amount         float64 `json:"amount"`
	CatalogVersion string  `json:"catalog_version"`
	EffectiveDate  string  `json:"effective_date"`
}

// RunSummary is the provider-comparison row derived from one complete profile run.
type RunSummary struct {
	Scored                   int               `json:"scored"`
	Passed                   int               `json:"passed"`
	Failed                   int               `json:"failed"`
	Unscored                 int               `json:"unscored"`
	MeanLatencyMilliseconds  int64             `json:"mean_latency_milliseconds"`
	P95LatencyMilliseconds   int64             `json:"p95_latency_milliseconds"`
	LLMRequests              uint64            `json:"llm_requests"`
	Tokens                   map[string]uint64 `json:"tokens"`
	ObservedProviderAndModel ProviderIdentity  `json:"observed_provider_and_model"`
	EstimatedCost            *CostSummary      `json:"estimated_cost,omitempty"`
}

// ProfileRun contains the full fixed-suite evidence for one selected model profile.
type ProfileRun struct {
	Profile        string           `json:"profile"`
	RequestedModel string           `json:"requested_model"`
	StartedAt      time.Time        `json:"started_at"`
	CompletedAt    time.Time        `json:"completed_at"`
	Results        []ScenarioResult `json:"results"`
	Summary        RunSummary       `json:"summary"`
}

// MetricsContract records the source-verified metric boundary behind a report.
type MetricsContract struct {
	AgentgatewayVersion string `json:"agentgateway_version"`
	TokenUsageMetric    string `json:"token_usage_metric"`
	CurrencyMetric      bool   `json:"currency_metric"`
	AttributionNote     string `json:"attribution_note"`
}

// Report is the mergeable machine output across comparable profile runs.
type Report struct {
	SchemaVersion string          `json:"schema_version"`
	GeneratedAt   time.Time       `json:"generated_at"`
	SuiteVersion  string          `json:"suite_version"`
	SuiteDigest   string          `json:"suite_digest"`
	Metrics       MetricsContract `json:"metrics_contract"`
	Runs          []ProfileRun    `json:"runs"`
}

// BuildSummary aggregates fixed-suite results and rejects provider identity drift.
func BuildSummary(results []ScenarioResult) (RunSummary, error) {
	if len(results) == 0 {
		return RunSummary{}, fmt.Errorf("cannot summarize an empty profile run")
	}
	summary := RunSummary{Tokens: make(map[string]uint64)}
	latencies := make([]int64, 0, len(results))
	identityKey := results[0].Usage.Identity.key()
	summary.ObservedProviderAndModel = results[0].Usage.Identity
	for _, result := range results {
		if result.Usage.Identity.key() != identityKey {
			return RunSummary{}, fmt.Errorf("profile run changed provider/model/route identity at scenario %q", result.ScenarioID)
		}
		switch result.Score.Verdict {
		case VerdictPass:
			summary.Scored++
			summary.Passed++
		case VerdictFail:
			summary.Scored++
			summary.Failed++
		case VerdictUnscored:
			summary.Unscored++
		default:
			return RunSummary{}, fmt.Errorf("scenario %q has unknown verdict %q", result.ScenarioID, result.Score.Verdict)
		}
		latencies = append(latencies, result.LatencyMilliseconds)
		summary.MeanLatencyMilliseconds += result.LatencyMilliseconds
		summary.LLMRequests += result.Usage.LLMRequests
		for tokenType, delta := range result.Usage.TokenTypes {
			summary.Tokens[tokenType] += delta.Tokens
		}
		if result.EstimatedCost != nil {
			if summary.EstimatedCost == nil {
				summary.EstimatedCost = &CostSummary{
					Currency:       result.EstimatedCost.Currency,
					CatalogVersion: result.EstimatedCost.CatalogVersion,
					EffectiveDate:  result.EstimatedCost.EffectiveDate,
				}
			}
			if summary.EstimatedCost.Currency != result.EstimatedCost.Currency ||
				summary.EstimatedCost.CatalogVersion != result.EstimatedCost.CatalogVersion ||
				summary.EstimatedCost.EffectiveDate != result.EstimatedCost.EffectiveDate {
				return RunSummary{}, fmt.Errorf("scenario %q used an inconsistent pricing catalog", result.ScenarioID)
			}
			summary.EstimatedCost.Amount += result.EstimatedCost.Amount
		}
	}
	summary.MeanLatencyMilliseconds /= int64(len(results))
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	rank := (95*len(latencies) + 99) / 100
	summary.P95LatencyMilliseconds = latencies[rank-1]
	return summary, nil
}

// NewReport returns an empty report pinned to the current scenario and metrics contracts.
func NewReport(now time.Time, digest string) Report {
	return Report{
		SchemaVersion: ReportSchemaVersion,
		GeneratedAt:   now.UTC(),
		SuiteVersion:  SuiteVersion,
		SuiteDigest:   digest,
		Metrics: MetricsContract{
			AgentgatewayVersion: AgentgatewayVersion,
			TokenUsageMetric:    TokenUsageMetric,
			CurrencyMetric:      false,
			AttributionNote:     "Direct Prometheus-exposition deltas are rejected when provider/model/route identities are ambiguous; same-series traffic inside one scenario window cannot be distinguished.",
		},
	}
}

// MergeRun adds or replaces a profile/model row while preserving comparability.
func MergeRun(report *Report, run ProfileRun) error {
	if report.SchemaVersion != ReportSchemaVersion {
		return fmt.Errorf("report schema_version = %q, want %q", report.SchemaVersion, ReportSchemaVersion)
	}
	if report.SuiteVersion != SuiteVersion {
		return fmt.Errorf("report suite_version = %q, want %q", report.SuiteVersion, SuiteVersion)
	}
	if report.SuiteDigest == "" {
		return fmt.Errorf("report suite_digest is empty")
	}
	if run.Profile == "" || run.RequestedModel == "" {
		return fmt.Errorf("profile and requested model are required")
	}
	for _, existing := range report.Runs {
		if existing.Summary.EstimatedCost != nil && run.Summary.EstimatedCost != nil {
			left, right := existing.Summary.EstimatedCost, run.Summary.EstimatedCost
			if left.Currency != right.Currency || left.CatalogVersion != right.CatalogVersion || left.EffectiveDate != right.EffectiveDate {
				return fmt.Errorf("priced profile runs must use the same currency and dated catalog version")
			}
		}
	}
	key := run.Profile + "\x00" + run.RequestedModel
	replaced := false
	for index := range report.Runs {
		if report.Runs[index].Profile+"\x00"+report.Runs[index].RequestedModel == key {
			report.Runs[index] = run
			replaced = true
			break
		}
	}
	if !replaced {
		report.Runs = append(report.Runs, run)
	}
	sort.Slice(report.Runs, func(i, j int) bool {
		if report.Runs[i].Profile == report.Runs[j].Profile {
			return report.Runs[i].RequestedModel < report.Runs[j].RequestedModel
		}
		return report.Runs[i].Profile < report.Runs[j].Profile
	})
	return nil
}

// LoadReport strictly decodes an existing machine report.
func LoadReport(path string) (Report, error) {
	encoded, err := os.ReadFile(path)
	if err != nil {
		return Report{}, fmt.Errorf("read report %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var report Report
	if err := decoder.Decode(&report); err != nil {
		return Report{}, fmt.Errorf("decode report %s: %w", path, err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Report{}, fmt.Errorf("decode report %s: %w", path, err)
	}
	return report, nil
}

// WriteReports atomically writes private JSON and Markdown outputs.
func WriteReports(report Report, jsonPath, markdownPath string) error {
	report.GeneratedAt = time.Now().UTC()
	encoded, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal evaluation report: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := writeAtomic(jsonPath, encoded); err != nil {
		return err
	}
	if err := writeAtomic(markdownPath, []byte(RenderMarkdown(report))); err != nil {
		return err
	}
	return nil
}

func writeAtomic(path string, content []byte) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return fmt.Errorf("create report directory %s: %w", directory, err)
	}
	temporary, err := os.CreateTemp(directory, ".evaluation-*")
	if err != nil {
		return fmt.Errorf("create temporary report: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("set report permissions: %w", err)
	}
	if _, err := temporary.Write(content); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write temporary report: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close temporary report: %w", err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace report %s: %w", path, err)
	}
	return nil
}

// RenderMarkdown produces the human provider-comparison table and its evidence boundaries.
func RenderMarkdown(report Report) string {
	var output bytes.Buffer
	fmt.Fprintln(&output, "# Fgentic model-profile evaluation")
	fmt.Fprintln(&output)
	fmt.Fprintf(&output, "Generated: %s  \n", report.GeneratedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&output, "Scenario suite: `%s` (`%s`)  \n", report.SuiteVersion, report.SuiteDigest)
	fmt.Fprintf(&output, "Metrics: agentgateway `%s` `%s`; no Prometheus currency metric exists.\n", report.Metrics.AgentgatewayVersion, report.Metrics.TokenUsageMetric)
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "| Profile | Requested model | Observed provider/model | Deterministic score | Mean / p95 latency | Input / output tokens | LLM calls | Estimated cost |")
	fmt.Fprintln(&output, "| --- | --- | --- | ---: | ---: | ---: | ---: | ---: |")
	for _, run := range report.Runs {
		summary := run.Summary
		observedModel := summary.ObservedProviderAndModel.ResponseModel
		if observedModel == "" || observedModel == "unknown" {
			observedModel = summary.ObservedProviderAndModel.RequestModel
		}
		cost := "not calculated"
		if summary.EstimatedCost != nil {
			cost = fmt.Sprintf("%.8f %s", summary.EstimatedCost.Amount, summary.EstimatedCost.Currency)
		}
		fmt.Fprintf(
			&output, "| %s | `%s` | `%s/%s` | %d/%d pass (%d optional unscored) | %d / %d ms | %d / %d | %d | %s |\n",
			markdownCell(run.Profile), markdownCell(run.RequestedModel),
			markdownCell(summary.ObservedProviderAndModel.System), markdownCell(observedModel),
			summary.Passed, summary.Scored, summary.Unscored,
			summary.MeanLatencyMilliseconds, summary.P95LatencyMilliseconds,
			summary.Tokens["input"], summary.Tokens["output"], summary.LLMRequests, cost,
		)
	}
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "Currency values are estimates from one operator-supplied, dated/versioned catalog; the provider invoice remains authoritative. Runs without such a catalog remain explicitly unpriced.")
	fmt.Fprintln(&output)
	fmt.Fprintln(&output, "Metric attribution fails when multiple provider/model/route identities change or traffic is observed in either quiet window. Aggregate metrics still cannot distinguish concurrent traffic on the exact same series during a scenario; run the suite on an otherwise idle profile.")
	return output.String()
}

func markdownCell(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "|", "\\|"), "\n", " ")
}
