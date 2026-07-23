// Package metrics holds the reconciler's Prometheus instruments. They measure GOVERNANCE flow —
// reconcile outcomes, grants, revocations, room-guard fail-closed events, the reconcile-stall alert,
// and the revocation-SLO alert — never model tokens (token metering stays aggregate at
// agentgateway). Labels are low-cardinality (outcome/reason), never a per-user MXID, so the series
// cannot leak who is in which room.
package metrics

import "github.com/prometheus/client_golang/prometheus"

// Metrics is the registered instrument set.
type Metrics struct {
	reconcile     *prometheus.CounterVec
	grants        *prometheus.CounterVec
	revocations   *prometheus.CounterVec
	guardFailures *prometheus.CounterVec
	stalled       prometheus.Gauge
	sloBreach     prometheus.Gauge
}

// New registers and returns the instrument set.
func New(reg prometheus.Registerer) *Metrics {
	m := &Metrics{
		reconcile: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "matrix_group_sync_reconcile_total",
			Help: "Reconcile cycles by outcome (applied|audit|partial|ambiguous).",
		}, []string{"outcome"}),
		grants: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "matrix_group_sync_grants_total",
			Help: "Grant decisions by outcome (invited|audit|skipped_no_account|skipped_invalid_localpart|blocked_room).",
		}, []string{"outcome"}),
		revocations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "matrix_group_sync_revocations_total",
			Help: "Revocation decisions by outcome (kicked|audit|failed).",
		}, []string{"outcome"}),
		guardFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "matrix_group_sync_room_guard_failures_total",
			Help: "Fail-closed room guards by reason (unresolved|unexpected_creator|room_version|power_drift|state_read).",
		}, []string{"reason"}),
		stalled: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "matrix_group_sync_reconcile_stalled",
			Help: "1 when consecutive incomplete/ambiguous cycles reached the alert threshold; else 0.",
		}),
		sloBreach: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "matrix_group_sync_revocation_slo_breach",
			Help: "1 when a computed revocation has been unapplied past the revocation SLO (enforce mode); else 0.",
		}),
	}
	reg.MustRegister(m.reconcile, m.grants, m.revocations, m.guardFailures, m.stalled, m.sloBreach)
	return m
}

// Reconcile records one cycle's outcome.
func (m *Metrics) Reconcile(outcome string) { m.reconcile.WithLabelValues(outcome).Inc() }

// Grant records one grant decision.
func (m *Metrics) Grant(outcome string) { m.grants.WithLabelValues(outcome).Inc() }

// Revocation records one revocation decision.
func (m *Metrics) Revocation(outcome string) { m.revocations.WithLabelValues(outcome).Inc() }

// GuardFailure records one fail-closed room guard.
func (m *Metrics) GuardFailure(reason string) { m.guardFailures.WithLabelValues(reason).Inc() }

// SetStalled raises or clears the reconcile-stall alert.
func (m *Metrics) SetStalled(on bool) { m.stalled.Set(boolValue(on)) }

// SetSLOBreach raises or clears the revocation-SLO alert.
func (m *Metrics) SetSLOBreach(on bool) { m.sloBreach.Set(boolValue(on)) }

func boolValue(on bool) float64 {
	if on {
		return 1
	}
	return 0
}
