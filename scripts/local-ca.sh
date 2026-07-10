#!/usr/bin/env bash
# Local development CA for the k3d cluster: generates a CA keypair (once), loads it as the
# `local-ca` Secret in the cert-manager namespace (backing the `local-ca` ClusterIssuer), and
# prints how to trust it on the host so browsers/curl accept https://*.fgentic.localhost.
# The keypair lives OUTSIDE the repo (~/.local/share/fgentic) — never committed.
set -euo pipefail

CA_DIR="${FGENTIC_CA_DIR:-${HOME}/.local/share/fgentic/local-ca}"
CA_CRT="${CA_DIR}/ca.crt"
CA_KEY="${CA_DIR}/ca.key"

if [ ! -f "${CA_CRT}" ]; then
  mkdir -p "${CA_DIR}"
  openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -sha256 -nodes \
    -keyout "${CA_KEY}" -out "${CA_CRT}" -days 3650 \
    -subj "/CN=Fgentic Local CA" \
    -addext "basicConstraints=critical,CA:TRUE" \
    -addext "keyUsage=critical,keyCertSign,cRLSign"
  chmod 600 "${CA_KEY}"
  echo "Generated local CA in ${CA_DIR}"
fi

kubectl get namespace cert-manager > /dev/null 2>&1 || kubectl create namespace cert-manager
secret_manifest="$(kubectl -n cert-manager create secret tls local-ca \
  --cert="${CA_CRT}" --key="${CA_KEY}" --dry-run=client -o yaml)"
kubectl apply -f - <<< "${secret_manifest}"
echo "Secret cert-manager/local-ca applied (ClusterIssuer local-ca is now usable)."

echo
echo "To trust it on this host (curl/browsers):"
echo "  sudo cp ${CA_CRT} /usr/local/share/ca-certificates/fgentic-local-ca.crt && sudo update-ca-certificates"
echo "or pass it explicitly: curl --cacert ${CA_CRT} https://chat.fgentic.localhost"
