package bridge

import (
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// resultMetadataKey is the custom top-level event-content field that carries the versioned,
// machine-readable A2A result block on a ghost's TERMINAL m.notice (#167). It lets a counterpart
// organisation's tooling parse an agent delegation's outcome structurally instead of scraping the
// human-readable notice body — the gematik-sanctioned "structured message metadata" pattern applied
// to cross-org agent collaboration.
//
// It is METADATA ONLY and does NOT change D8: the reply stays an m.notice, and the bridge (like any
// well-behaved automation) must never treat a notice as an instruction. Adding this field cannot make
// a notice actionable — bridge intake still ignores every ghost-authored notice (see
// TestResultMetadataDoesNotMakeNoticeActionable).
const resultMetadataKey = "ai.fgentic.a2a"

// resultMetadataVersion is the schema version of the ai.fgentic.a2a block. Bump it only on a breaking
// field change so a counterpart parser can pin exactly what it understands. It is marshalled as the
// block's "v" field.
const resultMetadataVersion = 1

// resultMetadata is the versioned, content-free result block attached to a terminal agent notice.
// It carries ONLY delegation identity and governance evidence the bridge already computed — never
// room message content, agent reply text, prompt text, or raw token counts. Every field is a stable
// identifier or a fixed enum, so the block cannot leak conversation content across an organisation
// boundary (D7/D8). The struct field order is fixed, so json.Marshal is deterministic.
type resultMetadata struct {
	// Version pins the schema (resultMetadataVersion) for counterpart parsers.
	Version int `json:"v"`
	// Outcome is the terminal delegation outcome, reusing the EXISTING metric/audit constants
	// (ok, failed, error, denied, timeout, lost, ...). It mirrors the fgentic_delegations_total label
	// and the fgentic.delegation.v1 audit "outcome" for the same delegation.
	Outcome string `json:"outcome"`
	// Agent is the ghost's FULL MXID @agent-<name>:<server> (D6 federation-safe identity), never a bare
	// localpart: a counterpart org must know which homeserver owns the answering agent.
	Agent string `json:"agent"`
	// TaskID is the A2A task identifier when the delegation reached A2A and the server minted one;
	// empty for a delegation refused before A2A (sender/stage denial, rate/budget limit) or a bare
	// terminal Message that carried no task. Omitted from JSON when empty.
	TaskID string `json:"task_id,omitempty"`
	// BudgetEvidence is a POINTER to the content-free audit stream where this delegation's cost and
	// outcome governance is recorded (the fgentic.delegation.v1 record, correlatable by agent + room +
	// task_id). It is deliberately a reference, NOT the token counts themselves: raw per-room spend
	// stays in-room (the budget command) and in the audit log, and is never projected into a federated
	// notice (D7/D8). The bridge does not mint a unique signed per-task receipt id here; a signed,
	// per-task usage receipt is the separate, provider-gated federation usage-receipt signer's job.
	BudgetEvidence string `json:"budget_evidence"`
}

// newResultMetadata builds the block for one terminal delegation from data the bridge already holds:
// the outcome constant and A2A task id the caller resolved, plus the ghost's full MXID derived from
// its localpart and the configured server name. The budget-evidence pointer is fixed to the
// delegation audit schema, the always-present content-free evidence record for the delegation.
func (b *Bridge) newResultMetadata(localpart, outcome, taskID string) *resultMetadata {
	return newResultMetadataForAgent(id.NewUserID(localpart, b.cfg.ServerName), outcome, taskID)
}

// newResultMetadataForAgent builds the block from an already-resolved full ghost MXID. It exists for
// callers that hold the MXID directly but not a *Bridge — the homeserver-fired dead-man notice, whose
// matrixDeadManClient has the ghost's intent identity but no bridge configuration.
func newResultMetadataForAgent(agent id.UserID, outcome, taskID string) *resultMetadata {
	return &resultMetadata{
		Version:        resultMetadataVersion,
		Outcome:        outcome,
		Agent:          agent.String(),
		TaskID:         taskID,
		BudgetEvidence: delegationAuditSchema,
	}
}

// automatedResultContent tags a TERMINAL agent notice with both the MSC3955 m.automated mixin
// (via automatedContent) and the versioned ai.fgentic.a2a result block. The block is layered onto the
// same additive Raw map, so mixin/block-unaware clients still render a plain m.notice unchanged.
//
// A nil meta yields exactly automatedContent: non-terminal replies (working placeholders, in-thread
// progress, the input-required question pause, the "only the original sender may answer" guard,
// command help, welcome) carry NO result block because they describe no terminal outcome.
//
// For an m.replace edit the block is mirrored into m.new_content as well — exactly like the automated
// mixin — so edit-aware clients keep it once they apply the replacement, while the top-level copy
// still reaches edit-unaware parsers.
func automatedResultContent(content *event.MessageEventContent, meta *resultMetadata) *event.Content {
	wrapped := automatedContent(content)
	if meta == nil {
		return wrapped
	}
	wrapped.Raw[resultMetadataKey] = meta
	if newContent, ok := wrapped.Raw[newContentKey].(map[string]any); ok {
		newContent[resultMetadataKey] = meta
	}
	return wrapped
}
