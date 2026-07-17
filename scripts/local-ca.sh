#!/usr/bin/env bash
# Local development CA for the k3d cluster: generates a CA keypair (once), loads it as the
# `local-ca` Secret in the cert-manager namespace (backing the `local-ca` ClusterIssuer), and
# prints how to trust it on the host so browsers/curl accept https://*.fgentic.localhost.
# The keypair lives OUTSIDE the repo (~/.local/share/fgentic) — never committed.
set -euo pipefail

CA_DIR="${FGENTIC_CA_DIR:-${HOME}/.local/share/fgentic/local-ca}"
CA_CRT="${CA_DIR}/ca.crt"
CA_KEY="${CA_DIR}/ca.key"
mode=apply
crt_present=no
key_present=no

case "${1:-}" in
"") ;;
--generate-only) mode=generate ;;
-h | --help)
  printf 'usage: scripts/local-ca.sh [--generate-only]\n'
  exit 0
  ;;
*)
  printf 'error: unsupported local CA option: %s\n' "$1" >&2
  exit 2
  ;;
esac
[ "$#" -le 1 ] || {
  echo 'error: local CA accepts at most one option' >&2
  exit 2
}

if [ -L "${CA_DIR}" ] || { [ -e "${CA_DIR}" ] && [ ! -d "${CA_DIR}" ]; }; then
  echo "error: local CA path must be a non-symlink directory: ${CA_DIR}" >&2
  exit 1
fi

[ ! -e "${CA_CRT}" ] && [ ! -L "${CA_CRT}" ] || crt_present=yes
[ ! -e "${CA_KEY}" ] && [ ! -L "${CA_KEY}" ] || key_present=yes
if [ "${crt_present}" != "${key_present}" ]; then
  echo "error: local CA keypair is incomplete in ${CA_DIR}" >&2
  exit 1
fi
if [ -e "${CA_CRT}" ]; then
  [ -f "${CA_CRT}" ] && [ ! -L "${CA_CRT}" ] &&
    [ -f "${CA_KEY}" ] && [ ! -L "${CA_KEY}" ] || {
    echo "error: local CA keypair must be regular non-symlink files in ${CA_DIR}" >&2
    exit 1
  }
  openssl x509 -in "${CA_CRT}" -noout >/dev/null 2>&1 || {
    echo "error: local CA certificate is invalid: ${CA_CRT}" >&2
    exit 1
  }
  openssl pkey -in "${CA_KEY}" -noout >/dev/null 2>&1 || {
    echo "error: local CA private key is invalid: ${CA_KEY}" >&2
    exit 1
  }
  cert_public="$(openssl x509 -in "${CA_CRT}" -pubkey -noout |
    openssl pkey -pubin -outform DER 2>/dev/null |
    openssl dgst -sha256 -r | awk '{print $1}')"
  key_public="$(openssl pkey -in "${CA_KEY}" -pubout -outform DER 2>/dev/null |
    openssl dgst -sha256 -r | awk '{print $1}')"
  if [ -z "${cert_public}" ] || [ "${cert_public}" != "${key_public}" ]; then
    echo "error: local CA certificate and private key do not match in ${CA_DIR}" >&2
    exit 1
  fi
fi

if [ ! -f "${CA_CRT}" ]; then
  mkdir -p "${CA_DIR}"
  openssl_config="$(mktemp "${TMPDIR:-/tmp}/fgentic-local-ca-openssl.XXXXXX")"
  trap 'rm -f "${openssl_config}"' EXIT INT TERM
  cat >"${openssl_config}" <<'EOF'
[req]
distinguished_name = subject
prompt = no
x509_extensions = v3_ca

[subject]
CN = Fgentic Local CA

[v3_ca]
basicConstraints = critical,CA:TRUE
keyUsage = critical,keyCertSign,cRLSign
EOF
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -sha256 -nodes \
    -keyout "${CA_KEY}" -out "${CA_CRT}" -days 3650 \
    -config "${openssl_config}" -extensions v3_ca
  chmod 600 "${CA_KEY}"
  echo "Generated local CA in ${CA_DIR}"
fi

if [ "${mode}" = generate ]; then
  echo "Local CA is ready in ${CA_DIR}; Kubernetes was not changed."
  exit 0
fi

kubectl get namespace cert-manager > /dev/null 2>&1 || kubectl create namespace cert-manager
secret_manifest="$(kubectl -n cert-manager create secret tls local-ca \
  --cert="${CA_CRT}" --key="${CA_KEY}" --dry-run=client -o yaml)"
kubectl apply -f - <<< "${secret_manifest}"
echo "Secret cert-manager/local-ca applied (ClusterIssuer local-ca is now usable)."

echo
echo "To trust it on this host (curl/browsers):"
case "$(uname -s)" in
Darwin)
  echo "  sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain ${CA_CRT}"
  ;;
Linux)
  echo "  sudo cp ${CA_CRT} /usr/local/share/ca-certificates/fgentic-local-ca.crt && sudo update-ca-certificates"
  ;;
*) echo "  import ${CA_CRT} into the host's trusted root certificate store" ;;
esac
echo "or pass it explicitly: curl --cacert ${CA_CRT} https://chat.fgentic.localhost"
