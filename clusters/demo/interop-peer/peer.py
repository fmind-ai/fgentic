#!/usr/bin/env python3
# Mastodon/GoToSocial-wire-compatible ActivityPub signing peer for the demo interop acceptance
# (issue #489, Task 7). It speaks the SAME wire protocol a real GoToSocial instance would — WebFinger
# discovery of an agent by handle, then Cavage HTTP-Signature-signed POSTs of a Follow and a
# Create(Note) mention to the agent inbox — but the harness owns the peer's RSA #main-key so it can
# deterministically re-POST a byte-identical signed activity for the at-least-once dedup proof (AC4),
# which a real GoToSocial binary never exposes. The gateway trusts this peer via an out-of-band PINNED
# key (ADR 0021), because an in-cluster peer cannot be SSRF-fetched by the #320-guarded resolver.
#
# Pure Python standard library only: no `cryptography`, no `openssl` CLI, no `curl`. RSA
# PKCS#1 v1.5 / SHA-256 signing is implemented directly over the PKCS#8 PEM key so the peer runs on a
# stock digest-pinned python:slim image with no network install step.
import base64
import email.utils
import hashlib
import json
import os
import sys
import urllib.request
from urllib.parse import urlparse

CONNECT_URL = os.environ["CONNECT_URL"].rstrip("/")  # in-cluster ClusterIP base, e.g. http://svc:8480
GHOST = os.environ["GHOST"]  # agent-docs-qa
AGENT_HANDLE = os.environ["AGENT_HANDLE"]  # agent-docs-qa@fgentic.localhost


# --- Minimal DER + RSA PKCS#1 v1.5 (SHA-256) signer, stdlib only ------------------------------------
def _read_tlv(data, off):
    tag = data[off]
    off += 1
    length = data[off]
    off += 1
    if length & 0x80:
        n = length & 0x7F
        length = int.from_bytes(data[off : off + n], "big")
        off += n
    return tag, data[off : off + length], off + length


def _read_int(data, off):
    tag, val, nxt = _read_tlv(data, off)
    assert tag == 0x02, "expected INTEGER"
    return int.from_bytes(val, "big"), nxt


def _rsa_key_from_pkcs8_pem(pem_text):
    body = "".join(
        line for line in pem_text.strip().splitlines() if "-----" not in line
    )
    der = base64.b64decode(body)
    # PrivateKeyInfo ::= SEQUENCE { version, algorithm SEQUENCE, privateKey OCTET STRING }
    _, seq, _ = _read_tlv(der, 0)
    off = 0
    _, off = _read_int(seq, off)  # version
    _, _, off = _read_tlv(seq, off)  # AlgorithmIdentifier SEQUENCE (skipped)
    _, pkcs1, _ = _read_tlv(seq, off)  # privateKey OCTET STRING -> PKCS#1 RSAPrivateKey
    # RSAPrivateKey ::= SEQUENCE { version, n, e, d, ... }
    _, inner, _ = _read_tlv(pkcs1, 0)
    o = 0
    _, o = _read_int(inner, o)  # version
    n, o = _read_int(inner, o)  # modulus
    _, o = _read_int(inner, o)  # publicExponent
    d, o = _read_int(inner, o)  # privateExponent
    return n, d


# ASN.1 DigestInfo prefix for SHA-256 (RFC 8017).
_SHA256_DIGESTINFO = bytes.fromhex("3031300d060960864801650304020105000420")


def rsa_sign_sha256(pem_text, message):
    n, d = _rsa_key_from_pkcs8_pem(pem_text)
    k = (n.bit_length() + 7) // 8
    digest_info = _SHA256_DIGESTINFO + hashlib.sha256(message).digest()
    ps = b"\xff" * (k - len(digest_info) - 3)
    em = b"\x00\x01" + ps + b"\x00" + digest_info
    signature = pow(int.from_bytes(em, "big"), d, n)
    return signature.to_bytes(k, "big")


# --- HTTP + Cavage signing --------------------------------------------------------------------------
def _agent_actor():
    """Discover the agent's actor URL by its fediverse handle via WebFinger (peer -> gateway)."""
    url = f"{CONNECT_URL}/.well-known/webfinger?resource=acct:{AGENT_HANDLE}"
    req = urllib.request.Request(url, headers={"Host": AGENT_HANDLE.split("@", 1)[1]})
    with urllib.request.urlopen(req, timeout=15) as resp:
        doc = json.load(resp)
    for link in doc.get("links", []):
        if link.get("rel") == "self":
            return link["href"]
    raise SystemExit("die: WebFinger returned no rel=self actor link for the agent handle")


def _sign_and_post(key_pem, key_id, actor, body_bytes, save_path=None):
    agent_actor = _agent_actor()
    parsed = urlparse(agent_actor)
    inbox_path = parsed.path + "/inbox"
    sign_host = parsed.hostname
    date = email.utils.formatdate(usegmt=True)
    digest = "SHA-256=" + base64.b64encode(hashlib.sha256(body_bytes).digest()).decode()
    signing_string = (
        f"(request-target): post {inbox_path}\n"
        f"host: {sign_host}\n"
        f"date: {date}\n"
        f"digest: {digest}"
    )
    sig = base64.b64encode(rsa_sign_sha256(key_pem, signing_string.encode())).decode()
    signature_header = (
        f'keyId="{key_id}",algorithm="rsa-sha256",'
        f'headers="(request-target) host date digest",signature="{sig}"'
    )
    headers = {
        "Host": sign_host,
        "Date": date,
        "Digest": digest,
        "Content-Type": "application/activity+json",
        "Signature": signature_header,
    }
    if save_path:  # persist the EXACT wire request so AC4 can replay it byte-for-byte
        with open(save_path, "w") as fh:
            json.dump({"path": inbox_path, "headers": headers, "body": body_bytes.decode()}, fh)
    return _post(inbox_path, headers, body_bytes)


def _post(path, headers, body_bytes):
    req = urllib.request.Request(f"{CONNECT_URL}{path}", data=body_bytes, headers=headers, method="POST")
    return _do(req)


def _do(req):
    try:
        with urllib.request.urlopen(req, timeout=30) as resp:
            return resp.status, resp.headers.get("Location", ""), resp.read()
    except urllib.error.HTTPError as err:
        return err.code, err.headers.get("Location", ""), err.read()


def _emit(status, location=b"", body=b""):
    loc = location.decode() if isinstance(location, bytes) else location
    print(f"STATUS {status}")
    if loc:
        print(f"LOCATION {loc}")
    if body:
        print("BODY " + (body.decode(errors="replace") if isinstance(body, bytes) else str(body)))


def cmd_discover():
    print(_agent_actor())


def cmd_follow(key_file, actor):
    agent_actor = _agent_actor()
    body = json.dumps(
        {
            "@context": "https://www.w3.org/ns/activitystreams",
            "id": f"{actor}/activities/follow-1",
            "type": "Follow",
            "actor": actor,
            "object": agent_actor,
        }
    ).encode()
    with open(key_file) as fh:
        key_pem = fh.read()
    status, loc, resp = _sign_and_post(key_pem, f"{actor}#main-key", actor, body)
    _emit(status, loc, resp)


def cmd_mention(key_file, actor, activity_id, save_path=None):
    agent_actor = _agent_actor()
    note_id = activity_id + "/note"
    body = json.dumps(
        {
            "@context": "https://www.w3.org/ns/activitystreams",
            "id": activity_id,
            "type": "Create",
            "actor": actor,
            "object": {
                "id": note_id,
                "type": "Note",
                "attributedTo": actor,
                "content": f"@{GHOST} please confirm the governed reply path works.",
                "tag": [{"type": "Mention", "href": agent_actor, "name": f"@{GHOST}"}],
            },
        }
    ).encode()
    with open(key_file) as fh:
        key_pem = fh.read()
    status, loc, resp = _sign_and_post(key_pem, f"{actor}#main-key", actor, body, save_path)
    _emit(status, loc, resp)


def cmd_replay(save_path):
    with open(save_path) as fh:
        saved = json.load(fh)
    req = urllib.request.Request(
        f"{CONNECT_URL}{saved['path']}",
        data=saved["body"].encode(),
        headers=saved["headers"],
        method="POST",
    )
    status, loc, resp = _do(req)
    _emit(status, loc, resp)


def cmd_poll(status_ref):
    path = status_ref if status_ref.startswith("/") else urlparse(status_ref).path
    req = urllib.request.Request(f"{CONNECT_URL}{path}", headers={"Host": AGENT_HANDLE.split("@", 1)[1]})
    status, _, resp = _do(req)
    state = "reply" if status == 200 else ""
    proof = ""
    try:
        doc = json.loads(resp)
        state = state or doc.get("state", "")
        if isinstance(doc, dict) and "proof" in doc:
            proof = "PROOF"  # FEP-8b32 object integrity proof present on the signed reply
    except (ValueError, TypeError):
        pass
    print(f"STATUS {status}")
    print(f"STATE {state}")
    if proof:
        print(proof)


def main():
    args = sys.argv[1:]
    if not args:
        raise SystemExit("die: no command")
    cmd, rest = args[0], args[1:]
    if cmd == "discover":
        cmd_discover()
    elif cmd == "follow":
        cmd_follow(rest[0], rest[1])
    elif cmd == "mention":
        cmd_mention(rest[0], rest[1], rest[2], rest[3] if len(rest) > 3 else None)
    elif cmd == "replay":
        cmd_replay(rest[0])
    elif cmd == "poll":
        cmd_poll(rest[0])
    else:
        raise SystemExit(f"die: unknown command {cmd}")


if __name__ == "__main__":
    main()
