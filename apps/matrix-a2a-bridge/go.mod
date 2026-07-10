module github.com/fmind/matrix-a2a-bridge

go 1.25.0

// NOTE: this environment has no module cache; run `go mod tidy` once to resolve go.sum (and,
// if a version below is stale, `go get maunium.net/go/mautrix@v0.28.1` /
// `go get github.com/a2aproject/a2a-go/v2@latest`). The code targets the APIs in those packages.
require (
	github.com/a2aproject/a2a-go/v2 v2.3.1
	github.com/caarlos0/env/v11 v11.3.1
	github.com/rs/zerolog v1.35.1
	gopkg.in/yaml.v3 v3.0.1
	maunium.net/go/mautrix v0.28.1
)

require (
	github.com/jackc/pgx/v5 v5.10.0
	go.mau.fi/util v0.9.10
	golang.org/x/time v0.15.0
)

require (
	filippo.io/edwards25519 v1.2.0 // indirect
	github.com/coder/websocket v1.8.15 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/mattn/go-colorable v0.1.14 // indirect
	github.com/mattn/go-isatty v0.0.20 // indirect
	github.com/petermattis/goid v0.0.0-20260330135022-df67b199bc81 // indirect
	github.com/tidwall/gjson v1.19.0 // indirect
	github.com/tidwall/match v1.1.1 // indirect
	github.com/tidwall/pretty v1.2.1 // indirect
	github.com/tidwall/sjson v1.2.5 // indirect
	golang.org/x/crypto v0.54.0 // indirect
	golang.org/x/exp v0.0.0-20260611194520-c48552f49976 // indirect
	golang.org/x/mod v0.38.0 // indirect
	golang.org/x/net v0.57.0 // indirect
	golang.org/x/sync v0.22.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/telemetry v0.0.0-20260708182218-49f421fb7959 // indirect
	golang.org/x/text v0.40.0 // indirect
	golang.org/x/tools v0.48.0 // indirect
	golang.org/x/vuln v1.6.0 // indirect
	mvdan.cc/gofumpt v0.10.0 // indirect
)

// Dev tools invoked via `go tool` (see mise.toml). `go mod tidy` fills their require versions.
tool (
	golang.org/x/tools/cmd/goimports
	golang.org/x/vuln/cmd/govulncheck
	mvdan.cc/gofumpt
)
