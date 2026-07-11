package evaluation

import (
	"strings"
	"testing"
)

const metricsFixture = `# HELP agentgateway_gen_ai_client_token_usage Number of tokens used per request
# TYPE agentgateway_gen_ai_client_token_usage histogram
agentgateway_gen_ai_client_token_usage_sum{gen_ai_token_type="input",gen_ai_system="openai",gen_ai_request_model="model-a",gen_ai_response_model="model-a-1",route="llm"} 12
agentgateway_gen_ai_client_token_usage_count{gen_ai_token_type="input",gen_ai_system="openai",gen_ai_request_model="model-a",gen_ai_response_model="model-a-1",route="llm"} 1
agentgateway_gen_ai_client_token_usage_sum{gen_ai_token_type="output",gen_ai_system="openai",gen_ai_request_model="model-a",gen_ai_response_model="model-a-1",route="llm"} 4
agentgateway_gen_ai_client_token_usage_count{gen_ai_token_type="output",gen_ai_system="openai",gen_ai_request_model="model-a",gen_ai_response_model="model-a-1",route="llm"} 1
`

func TestParseMetricsAndDelta(t *testing.T) {
	before, err := ParseMetrics(strings.NewReader(metricsFixture))
	if err != nil {
		t.Fatalf("ParseMetrics before: %v", err)
	}
	afterText := strings.NewReplacer(
		"} 12\n", "} 42\n",
		"} 4\n", "} 13\n",
		"} 1\n", "} 3\n",
	).Replace(metricsFixture)
	after, err := ParseMetrics(strings.NewReader(afterText))
	if err != nil {
		t.Fatalf("ParseMetrics after: %v", err)
	}
	delta, err := MetricsDelta(before, after)
	if err != nil {
		t.Fatalf("MetricsDelta: %v", err)
	}
	if delta.Identity.System != "openai" || delta.Identity.Route != "llm" {
		t.Fatalf("identity = %#v", delta.Identity)
	}
	if delta.LLMRequests != 2 || delta.TokenTypes["input"].Tokens != 30 || delta.TokenTypes["output"].Tokens != 9 {
		t.Fatalf("delta = %#v", delta)
	}
}

func TestMetricsDeltaRejectsAmbiguousIdentity(t *testing.T) {
	before, err := ParseMetrics(strings.NewReader(metricsFixture))
	if err != nil {
		t.Fatal(err)
	}
	second := strings.ReplaceAll(metricsFixture, "model-a", "model-b")
	metricOffset := strings.Index(second, "agentgateway_gen_ai_client_token_usage_sum")
	if metricOffset < 0 {
		t.Fatal("fixture has no token-usage sample")
	}
	second = second[metricOffset:]
	increased := strings.NewReplacer(
		"} 12\n", "} 13\n",
		"} 4\n", "} 5\n",
		"} 1\n", "} 2\n",
	).Replace(metricsFixture)
	after, err := ParseMetrics(strings.NewReader(increased + second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := MetricsDelta(before, after); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("MetricsDelta error = %v, want ambiguous attribution", err)
	}
}

func TestMetricsStableRejectsConcurrentNoise(t *testing.T) {
	before, err := ParseMetrics(strings.NewReader(metricsFixture))
	if err != nil {
		t.Fatal(err)
	}
	after, err := ParseMetrics(strings.NewReader(strings.Replace(metricsFixture, "} 12\n", "} 13\n", 1)))
	if err != nil {
		t.Fatal(err)
	}
	if err := MetricsStable(before, after); err == nil {
		t.Fatal("MetricsStable unexpectedly accepted changing metrics")
	}
}

func TestParseMetricsRequiresTokenHistogram(t *testing.T) {
	if _, err := ParseMetrics(strings.NewReader("# EOF\n")); err == nil {
		t.Fatal("ParseMetrics unexpectedly accepted missing metric")
	}
}
