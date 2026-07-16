# syntax=docker/dockerfile:1
# Multi-stage, static, distroless-nonroot image for the Matrix<->A2A bridge.
FROM golang:1.25 AS build
WORKDIR /src
# -mod=mod lets the build fetch + record sums even before a committed go.sum (run `go mod tidy`
# locally for a reproducible, pinned build). CGO off => a fully static binary for distroless.
ENV CGO_ENABLED=0 GOFLAGS=-mod=mod
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/bridge ./cmd/bridge && \
    go build -trimpath -ldflags="-s -w" -o /out/usage-receipt ./cmd/usage-receipt

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/fmind-ai/fgentic"
LABEL org.opencontainers.image.title="matrix-a2a-bridge"
LABEL org.opencontainers.image.description="Matrix Application Service that delegates @mentions to AI agents over A2A"
LABEL org.opencontainers.image.licenses="Apache-2.0"
# NOTICE ships in the image: the binary embeds MPL-2.0 mautrix/go (see NOTICE).
COPY NOTICE /usr/share/doc/matrix-a2a-bridge/NOTICE
COPY --from=build /out/bridge /usr/local/bin/bridge
COPY --from=build /out/usage-receipt /usr/local/bin/usage-receipt
USER 65532:65532
EXPOSE 29331
ENTRYPOINT ["/usr/local/bin/bridge"]
