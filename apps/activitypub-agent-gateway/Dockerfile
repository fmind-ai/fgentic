# syntax=docker/dockerfile:1
# Multi-stage, static, distroless-nonroot image for the ActivityPub agent gateway.
FROM golang:1.25 AS build
WORKDIR /src
# -mod=mod lets the build fetch + record sums even before a committed go.sum (run `go mod tidy`
# locally for a reproducible, pinned build). CGO off => a fully static binary for distroless.
ENV CGO_ENABLED=0 GOFLAGS=-mod=mod
COPY . .
RUN go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/fmind-ai/fgentic"
LABEL org.opencontainers.image.title="activitypub-agent-gateway"
LABEL org.opencontainers.image.description="ActivityPub Service actor per agent, delegating fediverse mentions to AI agents over A2A"
LABEL org.opencontainers.image.licenses="Apache-2.0"
# NOTICE ships in the image (Apache-2.0 attribution; all dependencies are permissive — see NOTICE).
COPY NOTICE /usr/share/doc/activitypub-agent-gateway/NOTICE
COPY --from=build /out/gateway /usr/local/bin/gateway
USER 65532:65532
EXPOSE 8480
ENTRYPOINT ["/usr/local/bin/gateway"]
