# Platform secret templates

The `*.sops.yaml.example` files document the shape of every Secret the platform needs. The REAL files live per cluster in `clusters/<env>/secrets/*.sops.yaml` (SOPS-encrypted, committed — Flux applies them from git): generate a full consistent set with `scripts/gen-secrets.sh <server_name> <env>`.
