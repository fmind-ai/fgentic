package evaluation

import (
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// TokenUsageMetric is the v1.3.1 agentgateway Prometheus histogram used for attribution.
const TokenUsageMetric = "agentgateway_gen_ai_client_token_usage"

const maxMetricsResponseBytes = 16 << 20

// ProviderIdentity is the stable provider, model, and route label set for one usage series.
type ProviderIdentity struct {
	System        string `json:"system"`
	RequestModel  string `json:"request_model"`
	ResponseModel string `json:"response_model,omitempty"`
	Bind          string `json:"bind,omitempty"`
	Gateway       string `json:"gateway,omitempty"`
	Listener      string `json:"listener,omitempty"`
	Route         string `json:"route,omitempty"`
	RouteRule     string `json:"route_rule,omitempty"`
	extraLabels   string
}

func (i ProviderIdentity) key() string {
	return strings.Join([]string{
		i.System, i.RequestModel, i.ResponseModel, i.Bind, i.Gateway, i.Listener, i.Route,
		i.RouteRule, i.extraLabels,
	}, "\x00")
}

type seriesKey struct {
	Identity  ProviderIdentity
	TokenType string
}

type metricValue struct {
	Requests uint64
	Tokens   float64
}

// MetricsSnapshot is one point-in-time view of all agentgateway token series.
type MetricsSnapshot struct {
	series map[seriesKey]metricValue
}

// TokenDelta holds the request observations and token total for one token type.
type TokenDelta struct {
	Requests uint64 `json:"requests"`
	Tokens   uint64 `json:"tokens"`
}

// UsageDelta attributes the changes around one scenario to a single provider identity.
type UsageDelta struct {
	Identity    ProviderIdentity      `json:"identity"`
	TokenTypes  map[string]TokenDelta `json:"token_types"`
	LLMRequests uint64                `json:"llm_requests"`
}

// MetricsReader captures point-in-time token-usage snapshots.
type MetricsReader interface {
	Snapshot(context.Context) (MetricsSnapshot, error)
}

// PrometheusReader reads the agentgateway Prometheus exposition endpoint directly.
type PrometheusReader struct {
	endpoint string
	client   *http.Client
}

// NewPrometheusReader validates endpoint and returns a bounded HTTP reader.
func NewPrometheusReader(endpoint string, timeout time.Duration) (*PrometheusReader, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return nil, fmt.Errorf("parse metrics URL: %w", err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("metrics URL scheme must be http or https")
	}
	if parsed.Host == "" {
		return nil, fmt.Errorf("metrics URL host is required")
	}
	return &PrometheusReader{
		endpoint: parsed.String(),
		client:   &http.Client{Timeout: timeout},
	}, nil
}

// Snapshot fetches and parses one token-usage snapshot.
func (r *PrometheusReader) Snapshot(ctx context.Context) (MetricsSnapshot, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.endpoint, nil)
	if err != nil {
		return MetricsSnapshot{}, fmt.Errorf("build metrics request: %w", err)
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return MetricsSnapshot{}, fmt.Errorf("fetch agentgateway metrics: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return MetricsSnapshot{}, fmt.Errorf("fetch agentgateway metrics: HTTP %s", resp.Status)
	}
	return ParseMetrics(io.LimitReader(resp.Body, maxMetricsResponseBytes))
}

// ParseMetrics extracts the required token histogram from Prometheus text exposition.
func ParseMetrics(input io.Reader) (MetricsSnapshot, error) {
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(input)
	if err != nil {
		return MetricsSnapshot{}, fmt.Errorf("parse Prometheus exposition: %w", err)
	}
	family := families[TokenUsageMetric]
	if family == nil {
		return MetricsSnapshot{}, fmt.Errorf("required metric %q is absent", TokenUsageMetric)
	}
	if family.GetType() != dto.MetricType_HISTOGRAM {
		return MetricsSnapshot{}, fmt.Errorf("metric %q is %s, want histogram", TokenUsageMetric, family.GetType())
	}

	snapshot := MetricsSnapshot{series: make(map[seriesKey]metricValue, len(family.Metric))}
	for _, metric := range family.Metric {
		if metric.Histogram == nil {
			return MetricsSnapshot{}, fmt.Errorf("metric %q contains a non-histogram sample", TokenUsageMetric)
		}
		labels := make(map[string]string, len(metric.Label))
		for _, pair := range metric.Label {
			labels[pair.GetName()] = pair.GetValue()
		}
		tokenType := labels["gen_ai_token_type"]
		if tokenType == "" {
			return MetricsSnapshot{}, fmt.Errorf("metric %q sample has no gen_ai_token_type", TokenUsageMetric)
		}
		identity := identityFromLabels(labels)
		key := seriesKey{Identity: identity, TokenType: tokenType}
		if _, duplicate := snapshot.series[key]; duplicate {
			return MetricsSnapshot{}, fmt.Errorf("metric %q has duplicate series for %s/%s", TokenUsageMetric, identity.System, tokenType)
		}
		snapshot.series[key] = metricValue{
			Requests: metric.Histogram.GetSampleCount(),
			Tokens:   metric.Histogram.GetSampleSum(),
		}
	}
	return snapshot, nil
}

func identityFromLabels(labels map[string]string) ProviderIdentity {
	known := map[string]struct{}{
		"gen_ai_token_type": {}, "gen_ai_system": {}, "gen_ai_request_model": {},
		"gen_ai_response_model": {}, "bind": {}, "gateway": {}, "listener": {}, "route": {},
		"route_rule": {},
	}
	extraKeys := make([]string, 0)
	for key := range labels {
		if _, found := known[key]; !found {
			extraKeys = append(extraKeys, key)
		}
	}
	sort.Strings(extraKeys)
	var extras strings.Builder
	for _, key := range extraKeys {
		extras.WriteString(key)
		extras.WriteByte('=')
		extras.WriteString(labels[key])
		extras.WriteByte('\x00')
	}
	return ProviderIdentity{
		System:        labels["gen_ai_system"],
		RequestModel:  labels["gen_ai_request_model"],
		ResponseModel: labels["gen_ai_response_model"],
		Bind:          labels["bind"],
		Gateway:       labels["gateway"],
		Listener:      labels["listener"],
		Route:         labels["route"],
		RouteRule:     labels["route_rule"],
		extraLabels:   extras.String(),
	}
}

// MetricsStable rejects any usage change in a quiet attribution window.
func MetricsStable(first, second MetricsSnapshot) error {
	if len(first.series) != len(second.series) {
		return fmt.Errorf("agentgateway token metrics changed during quiet window")
	}
	for key, before := range first.series {
		after, found := second.series[key]
		if !found || before.Requests != after.Requests || !nearlyEqual(before.Tokens, after.Tokens) {
			return fmt.Errorf("agentgateway token metrics changed during quiet window")
		}
	}
	return nil
}

// MetricsDelta calculates one scenario's usage and rejects ambiguous identities or resets.
func MetricsDelta(before, after MetricsSnapshot) (UsageDelta, error) {
	identities := make(map[string]ProviderIdentity)
	tokenTypes := make(map[string]TokenDelta)
	for key, end := range after.series {
		start := before.series[key]
		if end.Requests < start.Requests || end.Tokens+1e-9 < start.Tokens {
			return UsageDelta{}, fmt.Errorf("agentgateway token metric reset during scenario")
		}
		requestDelta := end.Requests - start.Requests
		tokenFloat := end.Tokens - start.Tokens
		if requestDelta == 0 && nearlyEqual(tokenFloat, 0) {
			continue
		}
		if requestDelta == 0 || tokenFloat < 0 || math.Abs(tokenFloat-math.Round(tokenFloat)) > 1e-6 {
			return UsageDelta{}, fmt.Errorf("invalid token metric delta for %q", key.TokenType)
		}
		identities[key.Identity.key()] = key.Identity
		tokenTypes[key.TokenType] = TokenDelta{Requests: requestDelta, Tokens: uint64(math.Round(tokenFloat))}
	}
	for key, start := range before.series {
		if _, found := after.series[key]; !found && (start.Requests != 0 || !nearlyEqual(start.Tokens, 0)) {
			return UsageDelta{}, fmt.Errorf("agentgateway token series disappeared during scenario")
		}
	}
	if len(identities) == 0 {
		return UsageDelta{}, fmt.Errorf("no agentgateway token usage observed for scenario")
	}
	if len(identities) != 1 {
		return UsageDelta{}, fmt.Errorf("ambiguous token attribution: %d provider/model/route identities changed", len(identities))
	}
	input, hasInput := tokenTypes["input"]
	output, hasOutput := tokenTypes["output"]
	if !hasInput || !hasOutput {
		return UsageDelta{}, fmt.Errorf("ambiguous token attribution: input and output series must both change")
	}
	if input.Requests != output.Requests {
		return UsageDelta{}, fmt.Errorf("ambiguous token attribution: input request delta %d differs from output %d", input.Requests, output.Requests)
	}
	var identity ProviderIdentity
	for _, candidate := range identities {
		identity = candidate
	}
	return UsageDelta{Identity: identity, TokenTypes: tokenTypes, LLMRequests: input.Requests}, nil
}

func nearlyEqual(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9
}
