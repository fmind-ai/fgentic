package evaluation

// FaithfulnessFixture pairs a corpus-cited answer with the per-claim entailment a correct sovereign
// judge would return and the expected content-free outcome. It is the retrieval-tool-agnostic contract
// the M25 retrieval path will populate live; here it is fixed evidence that lets the offline core prove
// the fail-closed and confident-but-unfaithful behaviors deterministically, with no model or corpus
// store. TrueEntailment is keyed by claim ID and drives a scripted judge in tests — a real judge is
// never run by the harness.
type FaithfulnessFixture struct {
	Name            string
	Answer          CitedAnswer
	TrueEntailment  map[string]bool
	ExpectedVerdict Verdict
	ExpectedClaims  map[string]FaithfulnessVerdict
}

// FaithfulnessFixtures returns the fixed citation-faithfulness cases exercised by the offline core:
// a fully supported answer, the confident-but-unfaithful negative case (a claim its cited chunk does
// not support), an uncited claim, and a claim whose cited chunk is missing from the provided set.
func FaithfulnessFixtures() []FaithfulnessFixture {
	return []FaithfulnessFixture{
		{
			Name: "supported-grounded-answer",
			Answer: CitedAnswer{
				Chunks: []CitedChunk{
					{ID: "docs/security.md#12", Text: "Room content is untrusted input to agents; prompt injection is the top threat."},
					{ID: "docs/bridge.md#6", Text: "Remote calls never receive the local A2A_API_KEY."},
				},
				Claims: []AnswerClaim{
					{ID: "c1", Text: "Agents treat room content as untrusted input.", CitedChunks: []string{"docs/security.md#12"}},
					{ID: "c2", Text: "A remote agent call is not given the local A2A API key.", CitedChunks: []string{"docs/bridge.md#6"}},
				},
			},
			TrueEntailment:  map[string]bool{"c1": true, "c2": true},
			ExpectedVerdict: VerdictPass,
			ExpectedClaims: map[string]FaithfulnessVerdict{
				"c1": FaithfulnessSupported,
				"c2": FaithfulnessSupported,
			},
		},
		{
			// The most dangerous failure: a confident claim its cited chunk does not actually support.
			Name: "confident-but-unfaithful",
			Answer: CitedAnswer{
				Chunks: []CitedChunk{
					{ID: "docs/design-decisions.md#7", Text: "Cross-org maxTokens is a per-azp admission reservation; actual model metrics remain aggregate."},
				},
				Claims: []AnswerClaim{
					{ID: "c1", Text: "The maxTokens value is the exact number of tokens each partner has already consumed.", CitedChunks: []string{"docs/design-decisions.md#7"}},
				},
			},
			TrueEntailment:  map[string]bool{"c1": false},
			ExpectedVerdict: VerdictFail,
			ExpectedClaims: map[string]FaithfulnessVerdict{
				"c1": FaithfulnessUnsupported,
			},
		},
		{
			// A claim that cites no chunk at all: its grounding cannot be verified, so it fails closed.
			Name: "uncited-claim",
			Answer: CitedAnswer{
				Chunks: []CitedChunk{
					{ID: "docs/federation.md#8", Text: "Federated rooms use room v12 and closed-federation allowlists."},
				},
				Claims: []AnswerClaim{
					{ID: "c1", Text: "Federated rooms use room v12.", CitedChunks: []string{"docs/federation.md#8"}},
					{ID: "c2", Text: "The platform supports two hundred concurrent homeservers.", CitedChunks: nil},
				},
			},
			TrueEntailment:  map[string]bool{"c1": true},
			ExpectedVerdict: VerdictFail,
			ExpectedClaims: map[string]FaithfulnessVerdict{
				"c1": FaithfulnessSupported,
				"c2": FaithfulnessUncited,
			},
		},
		{
			// A claim that cites a chunk ID absent from the provided set: unverifiable, fail closed.
			Name: "missing-cited-chunk",
			Answer: CitedAnswer{
				Chunks: []CitedChunk{
					{ID: "docs/licensing.md#10", Text: "Project code is Apache-2.0; the bridge takes no AGPL dependency."},
				},
				Claims: []AnswerClaim{
					{ID: "c1", Text: "The bridge image is signed with cosign.", CitedChunks: []string{"docs/observability.md#9"}},
				},
			},
			TrueEntailment:  map[string]bool{},
			ExpectedVerdict: VerdictFail,
			ExpectedClaims: map[string]FaithfulnessVerdict{
				"c1": FaithfulnessUnsupported,
			},
		},
	}
}
