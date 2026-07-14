package main

import (
	"strings"
	"testing"
)

func TestAssertAvailabilityStubStats(t *testing.T) {
	want := stubRequestRecord{Room: 99, Sequence: 1}
	stats := stubStats{
		HoldEnabled:    true,
		TotalRequests:  1,
		TotalStarted:   1,
		TotalCompleted: 1,
		Starts:         []stubRequestRecord{want},
		Completions:    []stubRequestRecord{want},
	}
	if err := assertAvailabilityStubStats(stats); err != nil {
		t.Fatalf("assertAvailabilityStubStats() error = %v", err)
	}

	stats.TotalRequests = 2
	if err := assertAvailabilityStubStats(stats); err == nil {
		t.Fatal("assertAvailabilityStubStats() accepted duplicate A2A request")
	}
}

func TestAssertReplacementMetrics(t *testing.T) {
	tests := []struct {
		name    string
		metrics bridgeMetrics
		wantErr string
	}{
		{
			name:    "exact replacement",
			metrics: bridgeMetrics{ProcessStartTime: 20, DedupSkips: 1},
		},
		{
			name:    "process changed",
			metrics: bridgeMetrics{ProcessStartTime: 21, DedupSkips: 1},
			wantErr: "process start time",
		},
		{
			name:    "extra replay",
			metrics: bridgeMetrics{ProcessStartTime: 20, DedupSkips: 2},
			wantErr: "dedup skips",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := assertReplacementMetrics(tt.metrics, 20, 1)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("assertReplacementMetrics() error = %v", err)
			}
			if tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("assertReplacementMetrics() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}
