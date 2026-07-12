# syntax=docker/dockerfile:1
# This image intentionally contains only the plain official-SDK A2A server. It proves that the
# bridge's runtime contract does not depend on kagent or on the bridge binary itself.
FROM docker.io/library/golang:1.25@sha256:d7912cedddfa15b2900a8dfb7187df0af5ec2cb424a371139b5b352fd3e6b740 AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
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
