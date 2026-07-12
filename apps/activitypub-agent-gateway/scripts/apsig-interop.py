# /// script
# requires-python = ">=3.11"
# dependencies = ["apsig", "cryptography", "multiformats"]
# ///
"""Independent FEP-8b32 interop check against the apsig reference implementation (issue #212).

This re-derives the golden eddsa-jcs-2022 vector the Go integrity package pins in
internal/integrity/integrity_test.go and asserts:

  1. apsig signing the same document with the same key reproduces the committed proofValue
     (so the Go signer, which reproduces it byte-for-byte, is interop-compatible with apsig), and
  2. apsig verifies that proof (so a real remote verifier accepts a Fgentic agent's signed reply).

It is NOT part of `mise run check`/`test` (those stay offline and deterministic). Run it on demand to
re-confirm interop after touching the signing path:  mise run interop

Regenerate the Go golden constants from the printed values if the fixture ever changes.
"""

import copy

from apsig import ProofSigner, ProofVerifier
from cryptography.hazmat.primitives import serialization
from cryptography.hazmat.primitives.asymmetric import ed25519
from multiformats import multibase, multicodec

# These MUST match the constants in internal/integrity/integrity_test.go.
GOLDEN_PUBLIC_KEY_MULTIBASE = "z6MkehRgf7yJbgaGfYsdoAsKdBPE3dj2CYhowQdcjqSJgvVd"
GOLDEN_PROOF_VALUE = "zqH9E5s2H9ZEM3iqaxSfSpvgD72R3VvHRAcetSFyssYcZQPtuHvro7EUWzHS8oH4EutM3KjH2KhPzpuW5jvgHLSe"

SEED = bytes(range(32))  # deterministic 00 01 02 ... 1f (matches goldenKey in Go)
priv = ed25519.Ed25519PrivateKey.from_private_bytes(SEED)
pub = priv.public_key()
raw_pub = pub.public_bytes(
    encoding=serialization.Encoding.Raw, format=serialization.PublicFormat.Raw
)
pub_multibase = multibase.encode(multicodec.wrap("ed25519-pub", raw_pub), "base58btc")

DOC = {
    "@context": [
        "https://www.w3.org/ns/activitystreams",
        "https://w3id.org/security/data-integrity/v1",
    ],
    "id": "https://fgentic.localhost/ap/agents/agent-docs-qa/activities/1",
    "type": "Create",
    "actor": "https://fgentic.localhost/ap/agents/agent-docs-qa",
    "to": ["https://mastodon.example/users/bob"],
    "published": "2026-07-12T09:00:00Z",
    "object": {
        "id": "https://fgentic.localhost/ap/agents/agent-docs-qa/objects/1",
        "type": "Note",
        "attributedTo": "https://fgentic.localhost/ap/agents/agent-docs-qa",
        "content": "Fgentic is a sovereignty-first agent platform.",
        "inReplyTo": "https://mastodon.example/notes/1",
        "to": ["https://mastodon.example/users/bob"],
        "published": "2026-07-12T09:00:00Z",
    },
}
OPTIONS = {
    "type": "DataIntegrityProof",
    "cryptosuite": "eddsa-jcs-2022",
    "verificationMethod": "https://fgentic.localhost/ap/agents/agent-docs-qa#ed25519-key",
    "created": "2026-07-12T09:00:00Z",
    "proofPurpose": "assertionMethod",
}


def main() -> None:
    assert pub_multibase == GOLDEN_PUBLIC_KEY_MULTIBASE, (
        f"publicKeyMultibase drift: {pub_multibase} != {GOLDEN_PUBLIC_KEY_MULTIBASE}"
    )

    signed = ProofSigner(priv).sign(DOC, OPTIONS)
    proof_value = signed["proof"]["proofValue"]
    assert proof_value == GOLDEN_PROOF_VALUE, (
        f"proofValue drift: {proof_value} != {GOLDEN_PROOF_VALUE} — Go and apsig disagree"
    )

    # deep-copy: ProofVerifier.verify() mutates the proof dict in place.
    verified = ProofVerifier(pub_multibase).verify(copy.deepcopy(signed))
    assert verified == OPTIONS["verificationMethod"], f"apsig rejected the proof: {verified}"

    print("apsig interop OK:")
    print(f"  publicKeyMultibase: {pub_multibase}")
    print(f"  proofValue:         {proof_value}")
    print("  apsig verified the eddsa-jcs-2022 proof and it matches the Go golden vector.")


if __name__ == "__main__":
    main()
