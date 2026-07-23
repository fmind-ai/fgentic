package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestMetricsRecord(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)

	m.Reconcile("applied")
	m.Grant("invited")
	m.Grant("invited")
	m.Revocation("kicked")
	m.GuardFailure("power_drift")
	m.SetStalled(true)
	m.SetSLOBreach(false)

	if got := testutil.ToFloat64(m.grants.WithLabelValues("invited")); got != 2 {
		t.Fatalf("expected 2 invited grants, got %v", got)
	}
	if got := testutil.ToFloat64(m.stalled); got != 1 {
		t.Fatalf("expected stall gauge raised, got %v", got)
	}
	if got := testutil.ToFloat64(m.sloBreach); got != 0 {
		t.Fatalf("expected SLO gauge cleared, got %v", got)
	}
}

func TestMetricsRegistered(t *testing.T) {
	reg := prometheus.NewRegistry()
	m := New(reg)
	// A counter family only appears in Gather once it has an observed label set; emit one of each.
	m.Reconcile("applied")
	m.Grant("invited")
	m.Revocation("kicked")
	m.GuardFailure("power_drift")
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"matrix_group_sync_reconcile_total":           false,
		"matrix_group_sync_grants_total":              false,
		"matrix_group_sync_revocations_total":         false,
		"matrix_group_sync_room_guard_failures_total": false,
		"matrix_group_sync_reconcile_stalled":         false,
		"matrix_group_sync_revocation_slo_breach":     false,
	}
	for _, f := range families {
		if _, ok := want[f.GetName()]; ok {
			want[f.GetName()] = true
		}
	}
	for name, seen := range want {
		if !seen && !strings.HasPrefix(name, "go_") {
			t.Errorf("metric %q was not registered", name)
		}
	}
}
