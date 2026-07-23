package evaluation

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// FaithfulnessVerdict is the validated per-claim citation-faithfulness outcome. A claim is either
// supported by its cited chunk(s), unsupported (the confident-but-unfaithful failure, or any
// fail-closed case such as a missing cited chunk or an unparseable judgment), or uncited (it cites no
// chunk at all). There is no silent-pass value.
type FaithfulnessVerdict string

const (
	// FaithfulnessSupported means the claim is entailed by the concatenated text of the chunks it cites,
	// as judged by the sovereign judge lane.
	FaithfulnessSupported FaithfulnessVerdict = "supported"
	// FaithfulnessUnsupported is the fail-closed verdict: the cited chunk does not entail the claim, a
	// cited chunk is missing, or the judgment could not be parsed. It is never a default silent pass.
	FaithfulnessUnsupported FaithfulnessVerdict = "unsupported"
	// FaithfulnessUncited means the claim cites no chunk, so its grounding cannot be verified.
	FaithfulnessUncited FaithfulnessVerdict = "uncited-claim"
)

// Valid reports whether v is one of the three defined faithfulness verdicts.
func (v FaithfulnessVerdict) Valid() bool {
	switch v {
	case FaithfulnessSupported, FaithfulnessUnsupported, FaithfulnessUncited:
		return true
	default:
		return false
	}
}

// CitedChunk is one corpus chunk an answer references: a stable ID and its sovereign text. The text
// reaches only the in-cluster judge lane (agentgateway) — never an external provider, a metric label,
// a report field, or a log line.
type CitedChunk struct {
	ID   string
	Text string
}

// AnswerClaim is one atomic assertion extracted from a corpus-cited answer, paired with the chunk IDs
// it cites. Text is sovereign and reaches only the judge lane; only ID and CitedChunks are ever
// surfaced in a result.
type AnswerClaim struct {
	ID          string
	Text        string
	CitedChunks []string
}

// CitedAnswer is the generic, retrieval-tool-agnostic faithfulness input: an answer decomposed into
// claims plus the corpus chunks those claims reference. The M25 retrieval tool populates it in the
// live path; fixtures provide it directly for the offline core. It carries no Matrix or transport
// coupling, so any retrieval tool that yields (claim, cited chunk IDs, chunk text) can drive the check.
type CitedAnswer struct {
	Claims []AnswerClaim
	Chunks []CitedChunk
}

// Validate rejects structurally malformed input fail-fast (empty or duplicate claim/chunk IDs). It
// deliberately permits an answer with no claims: an answer that cites nothing is a fail-closed verdict
// the harness must record, not an error it swallows.
func (a CitedAnswer) Validate() error {
	seenChunks := make(map[string]struct{}, len(a.Chunks))
	for _, chunk := range a.Chunks {
		if strings.TrimSpace(chunk.ID) == "" {
			return fmt.Errorf("cited chunk ID must be non-empty")
		}
		if _, duplicate := seenChunks[chunk.ID]; duplicate {
			return fmt.Errorf("duplicate cited chunk ID %q", chunk.ID)
		}
		seenChunks[chunk.ID] = struct{}{}
	}
	seenClaims := make(map[string]struct{}, len(a.Claims))
	for _, claim := range a.Claims {
		if strings.TrimSpace(claim.ID) == "" {
			return fmt.Errorf("answer claim ID must be non-empty")
		}
		if _, duplicate := seenClaims[claim.ID]; duplicate {
			return fmt.Errorf("duplicate answer claim ID %q", claim.ID)
		}
		seenClaims[claim.ID] = struct{}{}
		for _, cited := range claim.CitedChunks {
			if strings.TrimSpace(cited) == "" {
				return fmt.Errorf("claim %q cites a blank chunk ID", claim.ID)
			}
		}
	}
	return nil
}

// ClaimFaithfulness is the content-free faithfulness verdict for one claim: its stable ID, the
// verdict, and the chunk IDs it cited. It never carries the claim prose or any chunk text.
type ClaimFaithfulness struct {
	ClaimID     string              `json:"claim_id"`
	Verdict     FaithfulnessVerdict `json:"verdict"`
	CitedChunks []string            `json:"cited_chunks"`
}

// FaithfulnessResult is the validated, content-free per-answer citation-faithfulness outcome. The
// overall Verdict is VerdictPass only when the answer has at least one claim and every claim is
// supported; any unsupported or uncited claim, a missing cited chunk, an unparseable judgment, or an
// answer that cites nothing yields VerdictFail. It surfaces only verdicts and claim/chunk IDs.
type FaithfulnessResult struct {
	Verdict Verdict             `json:"verdict"`
	Claims  []ClaimFaithfulness `json:"claims"`
}

// EntailmentResult is the validated verdict of one sovereign entailment judgment: whether a single
// claim is entailed by the concatenated text of the chunks it cites. It is parsed fail-closed at the
// boundary from the judge model's strict JSON output. The rationale justifies the parse and is never
// persisted, keeping recorded evidence payload-free.
type EntailmentResult struct {
	Entailed  bool
	Rationale string
}

// entailmentWireResult is the exact on-the-wire contract the judge model must emit. Strict decoding
// with DisallowUnknownFields means any extra prose, missing field, or wrong shape fails closed rather
// than being read as an entailed (supported) claim.
type entailmentWireResult struct {
	Entailed  *bool  `json:"entailed"`
	Rationale string `json:"rationale"`
}

// ParseEntailmentResult parses one sovereign judge entailment response into a trusted result at the
// boundary. It fails closed on any deviation — non-JSON output, extra or missing fields, a non-bool
// entailed value, trailing content, or an empty rationale — so a malformed judgment can never be read
// as "supported". Every returned error is content-free so a caller can log it without leaking model
// output (the #355 payload-free invariant).
func ParseEntailmentResult(raw string) (EntailmentResult, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return EntailmentResult{}, fmt.Errorf("entailment judge returned an empty response")
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(trimmed)))
	decoder.DisallowUnknownFields()
	var wire entailmentWireResult
	if err := decoder.Decode(&wire); err != nil {
		// The raw json error can embed judge-authored content (an unknown field name, an offending
		// character), so it is deliberately dropped in favor of a fixed, content-free message.
		return EntailmentResult{}, fmt.Errorf("entailment result is not the expected JSON contract")
	}
	if decoder.More() {
		return EntailmentResult{}, fmt.Errorf("entailment result carries trailing content after the JSON object")
	}
	if wire.Entailed == nil {
		return EntailmentResult{}, fmt.Errorf("entailment result is missing the required entailed field")
	}
	if strings.TrimSpace(wire.Rationale) == "" {
		return EntailmentResult{}, fmt.Errorf("entailment result is missing a rationale")
	}
	return EntailmentResult{Entailed: *wire.Entailed, Rationale: wire.Rationale}, nil
}

// entailmentJudge decides whether a claim is entailed by its cited chunk texts. In production it is
// the sovereign judge lane (a local kagent Agent over agentgateway); tests supply a scripted verdict.
// A non-nil error is a transport/egress failure and must abort the run — it is never a verdict, so a
// judge outage can never be read as a silent pass.
type entailmentJudge func(ctx context.Context, claimText string, chunkTexts []string) (EntailmentResult, error)

// ScoreFaithfulness verifies every claim of a corpus-cited answer against the chunks it cites, using
// the sovereign judge for the entailment decision. It is fail-closed throughout:
//
//   - an answer with no claims yields VerdictFail (an answer that cites nothing never silently passes);
//   - a claim that cites no chunk is uncited-claim;
//   - a claim citing a chunk ID absent from the provided chunks is unsupported, with no judge call
//     (a chunk that cannot be read cannot be verified);
//   - a claim with empty text is unsupported (nothing to verify);
//   - otherwise the sovereign judge decides, and only a parsed, entailed verdict yields supported.
//
// A judge transport error is returned to the caller and must abort the run; it is never treated as a
// verdict. The returned result carries only verdicts and claim/chunk IDs — never claim or chunk text.
func ScoreFaithfulness(ctx context.Context, judge entailmentJudge, answer CitedAnswer) (FaithfulnessResult, error) {
	chunkText := make(map[string]string, len(answer.Chunks))
	for _, chunk := range answer.Chunks {
		chunkText[chunk.ID] = chunk.Text
	}
	result := FaithfulnessResult{Verdict: VerdictPass, Claims: make([]ClaimFaithfulness, 0, len(answer.Claims))}
	allSupported := true
	for _, claim := range answer.Claims {
		verdict, err := scoreClaim(ctx, judge, claim, chunkText)
		if err != nil {
			return FaithfulnessResult{}, err
		}
		if verdict != FaithfulnessSupported {
			allSupported = false
		}
		result.Claims = append(result.Claims, ClaimFaithfulness{
			ClaimID:     claim.ID,
			Verdict:     verdict,
			CitedChunks: append([]string(nil), claim.CitedChunks...),
		})
	}
	// Fail closed: an answer that cites nothing (no claims) or has any non-supported claim fails.
	if len(answer.Claims) == 0 || !allSupported {
		result.Verdict = VerdictFail
	}
	return result, nil
}

// scoreClaim resolves one claim to a fail-closed verdict, calling the judge only when the claim cites
// at least one present chunk and carries text to verify.
func scoreClaim(ctx context.Context, judge entailmentJudge, claim AnswerClaim, chunkText map[string]string) (FaithfulnessVerdict, error) {
	// A claim that cites no chunk is an uncited claim — fail closed, no egress.
	if len(claim.CitedChunks) == 0 {
		return FaithfulnessUncited, nil
	}
	// An empty claim has nothing to verify — fail closed, no egress.
	if strings.TrimSpace(claim.Text) == "" {
		return FaithfulnessUnsupported, nil
	}
	texts := make([]string, 0, len(claim.CitedChunks))
	for _, id := range claim.CitedChunks {
		text, found := chunkText[id]
		if !found {
			// A cited chunk that is not present cannot be verified — fail closed without a judge call.
			return FaithfulnessUnsupported, nil
		}
		if strings.TrimSpace(text) == "" {
			// A present-but-blank cited chunk is no evidence to entail against — fail closed without a
			// judge call, symmetric to the missing-chunk and empty-claim guards. Never let a judge
			// return "supported" on empty evidence.
			return FaithfulnessUnsupported, nil
		}
		texts = append(texts, text)
	}
	entail, err := judge(ctx, claim.Text, texts)
	if err != nil {
		return "", err
	}
	if entail.Entailed {
		return FaithfulnessSupported, nil
	}
	return FaithfulnessUnsupported, nil
}

// entailmentPrompt renders the fixed sovereign-judge instruction asking whether CLAIM is entailed by
// the CITED CHUNK text alone. It is a fixed rubric-driven template — the lane wiring, not the judge,
// owns the strict JSON contract ParseEntailmentResult accepts. Corpus and claim text appear only here,
// on the in-cluster judge egress.
func entailmentPrompt(claim string, chunks []string) string {
	var b strings.Builder
	b.WriteString("You are a strict, sovereign citation-faithfulness judge. Decide whether the CLAIM ")
	b.WriteString("is fully entailed (supported) by the CITED CHUNK text alone. Use no outside ")
	b.WriteString("knowledge. Respond with ONLY a single JSON object and no other text, exactly of the ")
	b.WriteString(`form {"entailed": <true|false>, "rationale": "<one sentence>"}. `)
	b.WriteString("Set entailed to true only if the cited text directly supports the claim.\n\n")
	b.WriteString("CLAIM: ")
	b.WriteString(claim)
	b.WriteString("\n\nCITED CHUNK:\n")
	for index, chunk := range chunks {
		fmt.Fprintf(&b, "[%d] %s\n", index+1, chunk)
	}
	return b.String()
}
