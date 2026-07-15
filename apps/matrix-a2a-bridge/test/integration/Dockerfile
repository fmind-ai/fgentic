# syntax=docker/dockerfile:1
# One image carries the production bridge and integration driver. The plain A2A agent has a
# separate image so the runtime-independence proof cannot accidentally depend on this binary.
FROM docker.io/library/golang:1.25@sha256:d7912cedddfa15b2900a8dfb7187df0af5ec2cb424a371139b5b352fd3e6b740 AS build
WORKDIR /src
ENV CGO_ENABLED=0
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY test/integration/cmd ./test/integration/cmd
RUN go build -trimpath -ldflags="-s -w" -o /out/bridge ./cmd/bridge \
  && go build -trimpath -ldflags="-s -w" -o /out/integration-driver ./test/integration/cmd/driver \
  && go build -trimpath -ldflags="-s -w" -o /out/fault-proxy ./test/integration/cmd/fault-proxy

FROM gcr.io/distroless/static-debian12:nonroot@sha256:b7bb25d9f7c31d2bdd1982feb4dafcaf137703c7075dbe2febb41c24212b946f
LABEL org.opencontainers.image.source="https://github.com/fmind-ai/fgentic"
LABEL org.opencontainers.image.title="matrix-a2a-bridge integration fixture"
LABEL org.opencontainers.image.licenses="Apache-2.0"
COPY NOTICE /usr/share/doc/matrix-a2a-bridge/NOTICE
COPY --from=build /out/bridge /usr/local/bin/bridge
COPY --from=build /out/integration-driver /usr/local/bin/integration-driver
COPY --from=build /out/fault-proxy /usr/local/bin/fault-proxy
USER 65532:65532
ENTRYPOINT ["/usr/local/bin/bridge"]
