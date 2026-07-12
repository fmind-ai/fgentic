package apgateway

import "github.com/prometheus/client_golang/prometheus"

// metrics are the gateway's aggregate counters. They deliberately measure GOVERNANCE flow
// (activities in, delegations out, rejections) and never model token usage — token metering stays
// aggregate at agentgateway, and cross-org budget admission lands with the federation border
// (docs/fediverse.md §6). All are labeled by ghost only (low cardinality), never by remote actor.
type metrics struct {
	inbound     *prometheus.CounterVec
	delegations *prometheus.CounterVec
	rejected    *prometheus.CounterVec
}

func newMetrics(reg prometheus.Registerer) *metrics {
	m := &metrics{
		inbound: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "apgateway_inbound_activities_total",
			Help: "ActivityPub inbox activities received, by ghost and activity type.",
		}, []string{"ghost", "type"}),
		delegations: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "apgateway_delegations_total",
			Help: "A2A delegations attempted from AP inbox mentions, by ghost and outcome.",
		}, []string{"ghost", "outcome"}),
		rejected: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "apgateway_rejected_total",
			Help: "Inbound requests rejected before any delegation, by reason.",
		}, []string{"reason"}),
	}
	reg.MustRegister(m.inbound, m.delegations, m.rejected)
	return m
}
