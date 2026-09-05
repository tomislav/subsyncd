# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.27.1
ARG DEBIAN_VERSION=13.2-slim

FROM golang:${GO_VERSION}-bookworm AS go-build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
RUN target_arch="${TARGETARCH:-$(go env GOARCH)}"; \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${target_arch} \
    go build -trimpath -ldflags="-s -w -X subsyncd/internal/version.Value=${VERSION}" -o /out/subsyncd ./cmd/subsyncd

FROM debian:${DEBIAN_VERSION} AS lapse-release
ARG TARGETARCH
ARG LAPSE_VERSION=2.0.5
ARG LAPSE_AMD64_SHA256=95f1eb35d83ee0084ba968f145c23c3c79aeb08c995b0f584b2177b016b67014
ARG LAPSE_ARM64_SHA256=23226fea64f7141687b764e5d080b6ed4f9e2fbed476938363e993bd3705ee17
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates curl && rm -rf /var/lib/apt/lists/*
RUN set -eux; \
    target_arch="${TARGETARCH:-$(dpkg --print-architecture)}"; \
    case "${target_arch}" in \
      amd64) archive="lapse-linux-amd64.tar.gz"; checksum="${LAPSE_AMD64_SHA256}" ;; \
      arm64) archive="lapse-linux-arm64.tar.gz"; checksum="${LAPSE_ARM64_SHA256}" ;; \
      *) echo "unsupported LAPSE architecture: ${target_arch}" >&2; exit 1 ;; \
    esac; \
    curl -fsSL --retry 3 -o /tmp/lapse.tar.gz "https://github.com/Schwponaco-org/lapse/releases/download/v${LAPSE_VERSION}/${archive}"; \
    echo "${checksum}  /tmp/lapse.tar.gz" | sha256sum -c -; \
    mkdir -p /out/lapse; \
    tar -xzf /tmp/lapse.tar.gz --strip-components=1 -C /out/lapse; \
    rm /tmp/lapse.tar.gz

FROM debian:${DEBIAN_VERSION} AS runtime
ARG VERSION=dev
ARG LAPSE_VERSION=2.0.5
LABEL org.opencontainers.image.title="subsyncd" \
      org.opencontainers.image.description="Focused, headless subtitle acquisition and synchronization service" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.lapse.version="${LAPSE_VERSION}"
RUN apt-get update && apt-get install -y --no-install-recommends \
      ca-certificates curl ffmpeg libfftw3-double3 tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 1000 subsyncd \
    && useradd --uid 1000 --gid 1000 --no-create-home --shell /usr/sbin/nologin subsyncd \
    && install -d -o 1000 -g 1000 -m 0750 /config /data /media
COPY --from=go-build /out/subsyncd /usr/local/bin/subsyncd
COPY --from=lapse-release /out/lapse /opt/lapse
RUN chmod 0755 /usr/local/bin/subsyncd /opt/lapse/lapse \
    && install -d /usr/share/doc/subsyncd /usr/share/licenses/lapse \
    && cp /opt/lapse/LICENSE /usr/share/licenses/lapse/LICENSE
COPY config.example.yaml /usr/share/doc/subsyncd/config.example.yaml
ENV LD_LIBRARY_PATH=/opt/lapse
USER 1000:1000
EXPOSE 8097
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["curl", "-fsS", "http://127.0.0.1:8097/readyz"]
ENTRYPOINT ["subsyncd"]
CMD ["serve", "--config", "/config/config.yaml"]
