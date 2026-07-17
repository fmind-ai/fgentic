package bridge

import (
	"context"

	"maunium.net/go/mautrix/event"

	"github.com/fmind-ai/matrix-a2a-bridge/internal/a2aclient"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/replyscan"
	"github.com/fmind-ai/matrix-a2a-bridge/internal/state"
)

// secretWithheldNotice is the bounded, content-free copy posted when block mode withholds a reply
// that appeared to carry a credential. It reuses the ghost notice plane and names no matched value.
const secretWithheldNotice = "⚠️ reply withheld — the agent reply appeared to contain a credential."

// deliverScannedResult applies the reply->room secret/credential leak scan (#343) to a terminal
// agent result before it is persisted to the durable outbox or projected to the room, then routes
// the delegation to its matching durable projection. Placement is load-bearing: the scan runs at
// this pre-persist chokepoint (before StateReplyPending) so a detected credential never reaches
// reply_pending, the ledger ResultText, a log, an audit record, or the timeline. In block mode the
// whole reply is withheld; in redact/annotate the masked result is persisted; a clean reply (or an
// off policy) is delivered byte-unchanged.
func (b *Bridge) deliverScannedResult(
	ctx context.Context,
	job *state.Job,
	payload durablePayload,
	evt *event.Event,
	ref *AgentRef,
	sender senderIdentity,
	result a2aclient.Result,
	terminalStage string,
) error {
	mode := b.replyScanMode(sender, ref)
	if mode == replyscan.ModeOff {
		return b.deliverCleanResult(ctx, job, payload, result, terminalStage)
	}

	// Scan every fragment folded into the visible reply: the text, each structured data block, and
	// each link label/URL. Raw file artifacts are byte parts governed separately by the media policy;
	// block mode withholds the whole result (and therefore any artifacts) regardless.
	masked, summary := replyscan.SanitizeAll(replyFragments(result))
	if summary.Clean() {
		return b.deliverCleanResult(ctx, job, payload, result, terminalStage)
	}

	// Emit only content-free evidence: the applied action, the masked-span count, and fixed rule
	// classes. The matched value is never logged, metered, or audited.
	b.log.Warn("reply secret scan detected credential material",
		"job_id", job.JobID, "ghost", job.GhostLocalpart, "action", mode.String(),
		"match_count", summary.Count, "rule_classes", summary.Classes)
	replySecretScans.WithLabelValues(job.GhostLocalpart, mode.String()).Inc()
	payload.Audit.SecretScanAction = mode.String()
	payload.Audit.SecretMatchCount = summary.Count
	payload.Audit.SecretRuleClasses = summary.Classes

	if mode == replyscan.ModeBlock {
		return b.prepareDurableDeniedNotice(
			ctx, job, payload, evt, ref, sender,
			secretWithheldNotice, terminalStage, errorSecretInReply,
		)
	}

	scanned := applyMaskedFragments(result, masked)
	if mode == replyscan.ModeAnnotate {
		scanned.Text = appendTransparencyNotice(scanned.Text, summary)
	}
	return b.prepareDurableResult(ctx, job, payload, scanned, state.StateDelivered,
		outcomeOK, terminalStage, "completed")
}

// deliverCleanResult persists and projects a result the scan did not alter, preserving the exact
// prior delivered-outcome contract.
func (b *Bridge) deliverCleanResult(
	ctx context.Context,
	job *state.Job,
	payload durablePayload,
	result a2aclient.Result,
	terminalStage string,
) error {
	return b.prepareDurableResult(ctx, job, payload, result, state.StateDelivered,
		outcomeOK, terminalStage, "completed")
}

// replyScanMode resolves the reply-scan posture for one delegation. A delegation shows federation
// exposure — and takes the stricter federated posture — when its sender is on a remote homeserver,
// arrived through a bridged origin, or is bound to a remote agent target. The bridge cannot read
// m.room.create / m.federate, so it keys off these bounded, contract-sanctioned signals
// (docs/audit.md, docs/federation.md §8.3) rather than inferring room-level federation, and
// validate() guarantees the federated posture is never weaker than the base. Because every non-off
// mode masks the matched value, a room whose federation the bridge cannot observe still never leaks
// a raw secret — detection only escalates mask→withhold.
func (b *Bridge) replyScanMode(sender senderIdentity, ref *AgentRef) replyscan.Mode {
	base := b.cfg.ReplyScanBaseMode()
	if b.federationExposed(sender, ref) {
		return b.cfg.ReplyScanFederatedPosture()
	}
	return base
}

// federationExposed reports whether a delegation carries any bounded signal that its reply could
// reach a homeserver other than this one. It never treats a partner-supplied MXID or homeserver as
// authenticated identity; it only uses these signals to select a stricter, fail-safe posture.
func (b *Bridge) federationExposed(sender senderIdentity, ref *AgentRef) bool {
	if sender.mxid.Homeserver() != b.cfg.ServerName {
		return true
	}
	if sender.isBridged() {
		return true
	}
	if ref != nil && ref.Target().IsRemote() {
		return true
	}
	return false
}

// replyFragments lists, in a fixed order, every text-bearing fragment folded into the visible reply
// so the scan sees exactly what a room member would: the reply text, each structured data block, and
// each link's label then URL. applyMaskedFragments consumes the same order to rebuild the result.
//
// Link fragments are normalized through the exact renderLinks transformation (sanitizeInline strips
// C0/DEL control characters and trims) BEFORE scanning. renderLinks applies that strip at delivery,
// so a control byte embedded in a link would otherwise break the scanner's contiguity anchors while
// the render-time strip silently reassembles a working credential in the posted event and the
// ledger. Text and data blocks are scanned raw because they are posted verbatim (no control strip),
// so a control-split token stays split in the room and cannot reassemble.
func replyFragments(result a2aclient.Result) []string {
	fragments := make([]string, 0, 1+len(result.Data)+2*len(result.Links))
	fragments = append(fragments, result.Text)
	fragments = append(fragments, result.Data...)
	for _, link := range result.Links {
		fragments = append(
			fragments,
			sanitizeInline(link.Label, maxFilenameRunes),
			sanitizeInline(link.URL, maxLinkChars),
		)
	}
	return fragments
}

// applyMaskedFragments rebuilds a result from the masked fragments produced for replyFragments,
// copying the Data and Links slices so the caller's original result is never mutated in place.
func applyMaskedFragments(result a2aclient.Result, masked []string) a2aclient.Result {
	i := 0
	result.Text = masked[i]
	i++
	if len(result.Data) > 0 {
		data := make([]string, len(result.Data))
		for d := range result.Data {
			data[d] = masked[i]
			i++
		}
		result.Data = data
	}
	if len(result.Links) > 0 {
		links := make([]a2aclient.ResultLink, len(result.Links))
		for l := range result.Links {
			links[l] = result.Links[l]
			links[l].Label = masked[i]
			i++
			links[l].URL = masked[i]
			i++
		}
		result.Links = links
	}
	return result
}

// appendTransparencyNotice appends the bounded, value-free annotate-mode notice to a reply body.
func appendTransparencyNotice(text string, summary replyscan.Result) string {
	notice := replyscan.TransparencyNotice(summary)
	switch {
	case notice == "":
		return text
	case text == "":
		return notice
	default:
		return text + "\n\n" + notice
	}
}
