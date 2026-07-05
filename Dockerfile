# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /miabi-runner .

# Cloud Native Buildpacks `pack` CLI, so the runner can build buildpack apps (not
# just Dockerfiles). pack publishes per-arch linux tarballs.
FROM alpine:3.21 AS pack
ARG PACK_VERSION=0.40.7
ARG TARGETARCH
# hadolint ignore=DL3018
RUN apk add --no-cache curl ca-certificates
RUN set -eux; \
    case "${TARGETARCH:-amd64}" in \
      amd64) asset="pack-v${PACK_VERSION}-linux.tgz" ;; \
      arm64) asset="pack-v${PACK_VERSION}-linux-arm64.tgz" ;; \
      *) echo "unsupported TARGETARCH: ${TARGETARCH}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL "https://github.com/buildpacks/pack/releases/download/v${PACK_VERSION}/${asset}" -o /tmp/pack.tgz; \
    tar -xzf /tmp/pack.tgz -C /usr/local/bin pack; \
    chmod +x /usr/local/bin/pack; \
    /usr/local/bin/pack version

FROM alpine:3.20
# Re-declare in this stage so ${VERSION} is in scope for the label below.
ARG VERSION=dev
LABEL org.opencontainers.image.title="Miabi Runner" \
      org.opencontainers.image.description="Dedicated build & pipeline execution runtime for Miabi: dials the control plane over an outbound WebSocket tunnel, leases build jobs, and builds/pushes images so hosting nodes only ever pull." \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.authors="Jonas Kaninda" \
      org.opencontainers.image.vendor="miabi-io" \
      org.opencontainers.image.url="https://github.com/miabi-io/runner" \
      org.opencontainers.image.source="https://github.com/miabi-io/runner" \
      org.opencontainers.image.documentation="https://github.com/miabi-io/runner#readme" \
      org.opencontainers.image.licenses="Apache-2.0"

RUN apk add --no-cache ca-certificates docker-cli docker-cli-buildx git
COPY --from=build /miabi-runner /usr/local/bin/miabi-runner
COPY --from=pack /usr/local/bin/pack /usr/local/bin/pack
ENTRYPOINT ["miabi-runner"]
