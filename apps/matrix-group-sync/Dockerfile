# syntax=docker/dockerfile:1
# Multi-stage, static, distroless-nonroot image for the matrix-group-sync reconciler.
FROM golang:1.25 AS build
WORKDIR /src
# -mod=mod lets the build fetch + record sums even before a committed go.sum (run `go mod tidy`
# locally for a reproducible, pinned build). CGO off => a fully static binary for distroless.
ENV CGO_ENABLED=0 GOFLAGS=-mod=mod
COPY . .
RUN go build -trimpath -buildvcs=false -ldflags="-s -w" -o /out/matrix-group-sync ./cmd/matrix-group-sync

FROM gcr.io/distroless/static-debian12:nonroot
LABEL org.opencontainers.image.source="https://github.com/fmind-ai/fgentic"
LABEL org.opencontainers.image.title="matrix-group-sync"
LABEL org.opencontainers.image.description="GitOps reconciler materializing IdP-group membership into managed Matrix room membership"
LABEL org.opencontainers.image.licenses="Apache-2.0"
# NOTICE ships in the image (Apache-2.0 attribution; all dependencies are permissive — see NOTICE).
COPY NOTICE /usr/share/doc/matrix-group-sync/NOTICE
COPY --from=build /out/matrix-group-sync /usr/local/bin/matrix-group-sync
USER 65532:65532
EXPOSE 9090
ENTRYPOINT ["/usr/local/bin/matrix-group-sync"]
