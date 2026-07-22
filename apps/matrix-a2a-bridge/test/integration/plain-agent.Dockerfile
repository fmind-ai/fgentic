# syntax=docker/dockerfile:1
# This image intentionally contains only the plain official-SDK A2A server. It proves that the
# bridge's runtime contract does not depend on kagent or on the bridge binary itself.
FROM docker.io/library/golang:1.26@sha256:3aff6657219a4d9c14e27fb1d8976c49c29fddb70ba835014f477e1c70636647 AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY internal/agentcardjws ./internal/agentcardjws
COPY test/integration/cmd/a2a-stub ./test/integration/cmd/a2a-stub
RUN go build -trimpath -ldflags="-s -w" -o /out/plain-a2a-agent ./test/integration/cmd/a2a-stub

FROM gcr.io/distroless/static-debian12:nonroot@sha256:b7bb25d9f7c31d2bdd1982feb4dafcaf137703c7075dbe2febb41c24212b946f
LABEL org.opencontainers.image.source="https://github.com/fmind-ai/fgentic"
LABEL org.opencontainers.image.title="plain a2a-go runtime-independence fixture"
LABEL org.opencontainers.image.licenses="Apache-2.0"
COPY NOTICE /usr/share/doc/plain-a2a-agent/NOTICE
COPY --from=build /out/plain-a2a-agent /usr/local/bin/plain-a2a-agent
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/plain-a2a-agent"]
