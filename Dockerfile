# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /miabi-runner .

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
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 runner
COPY --from=build /miabi-runner /usr/local/bin/miabi-runner
USER runner
ENTRYPOINT ["miabi-runner"]
