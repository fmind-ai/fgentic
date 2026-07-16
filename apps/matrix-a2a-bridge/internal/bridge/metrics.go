package bridge

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Prometheus instrumentation (SPEC §9.3): delegation outcomes, A2A latency, queue pressure,
// and dedup effectiveness — served on the METRICS_PORT side port (cmd/bridge).
var (
	delegationsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fgentic_delegations_total",
		Help: "Delegation attempts addressed to agent ghosts, by ghost and outcome.",
	}, []string{"ghost", "outcome"})

	a2aLatency = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "fgentic_a2a_request_seconds",
		Help: "Latency of A2A SendMessage round trips (excludes long-task polling).",
		// LLM-backed calls run seconds to minutes.
		Buckets: []float64{0.5, 1, 2, 5, 10, 30, 60, 120},
	}, []string{"ghost"})

	inflightDelegations = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "fgentic_inflight_delegations",
		Help: "Delegations currently running on the dispatcher worker pool.",
	})

	queueDepth = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "fgentic_queue_depth",
		Help: "Delegations currently queued across all rooms.",
	})

	dedupSkipsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "fgentic_dedup_skips_total",
		Help: "Events skipped because the homeserver redelivered an already-processed transaction.",
	})

	durableStateTransitions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fgentic_delegation_ledger_transitions_total",
		Help: "Content-free durable delegation state transitions.",
	}, []string{"from_state", "to_state"})

	durableRecoveryOutcomes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fgentic_delegation_recovery_outcomes_total",
		Help: "Hard-crash recovery outcomes requiring operator attention.",
	}, []string{"outcome"})

	agentReplyQualitySignals = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "fgentic_agent_reply_quality_signals_total",
		Help: "Content-free human quality reactions on known terminal agent replies.",
	}, []string{"ghost", "signal"})
)

// Outcome labels for fgentic_delegations_total.
const (
	outcomeOK            = "ok"
	outcomeFailed        = "failed"         // agent task ended failed/canceled/rejected
	outcomeError         = "error"          // A2A transport/protocol error
	outcomeRateLimited   = "rate_limited"   // D7 budget rejection
	outcomeDenied        = "denied"         // sender policy rejection before A2A
	outcomeQueueFull     = "queue_full"     // bounded dispatcher rejected before admission
	outcomeShutdown      = "shutdown"       // target did not start before dispatcher shutdown
	outcomeTimeout       = "timeout"        // long task exceeded TASK_TIMEOUT
	outcomeLost          = "lost"           // GetTask error budget exhausted
	outcomeCanceled      = "canceled"       // long task canceled from the room (#98)
	outcomeInputRequired = "input_required" // task paused awaiting a threaded reply (#116)
	outcomeAmbiguous     = "ambiguous"      // A2A may have accepted a request whose ACK was lost
	outcomeDead          = "dead"           // bounded recovery exhausted; operator action required
)
