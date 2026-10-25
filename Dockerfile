# syntax=docker/dockerfile:1.7

ARG GO_VERSION=1.27.1
ARG UBUNTU_VERSION=26.04
ARG UBUNTU_DIGEST=sha256:c098726dd9b9733f3e885f8de0ef375206a311b0a317eadc40d347b1115fb944

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

FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS ffprobe-build
ARG TARGETARCH
ARG FFMPEG_VERSION=8.1
ARG FFMPEG_SHA256=b072aed6871998cce9b36e7774033105ca29e33632be5b6347f3206898e0756a
RUN apk add --no-cache build-base clang lld nasm linux-headers pkgconf curl
COPY scripts/build-ffprobe.sh /build-ffprobe.sh
RUN sh /build-ffprobe.sh "${FFMPEG_VERSION}" "${FFMPEG_SHA256}" "${TARGETARCH}"

FROM ubuntu:${UBUNTU_VERSION}@${UBUNTU_DIGEST} AS lapse-release
ARG TARGETARCH
ARG LAPSE_VERSION=2.3.1
ARG LAPSE_AMD64_SHA256=029f0b178298728a416b9d6feb146a6546fa08f8d84c1a29bccbce3136e95a86
ARG LAPSE_ARM64_SHA256=f34a499f7a15d4a86de07b9293fda2505eba363b7bb305c9ad9f2a9f6fcecd90
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

FROM ubuntu:${UBUNTU_VERSION}@${UBUNTU_DIGEST} AS runtime
ARG FFMPEG_VERSION=8.1
ARG VERSION=dev
ARG LAPSE_VERSION=2.3.1
LABEL org.opencontainers.image.title="subsyncd" \
      org.opencontainers.image.description="Focused, headless subtitle acquisition and synchronization service" \
      org.opencontainers.image.version="${VERSION}" \
      org.opencontainers.image.lapse.version="${LAPSE_VERSION}" \
      org.opencontainers.image.ffmpeg.version="${FFMPEG_VERSION}"
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      ca-certificates curl libfftw3-double3 libstdc++6 tzdata \
    && rm -rf /var/lib/apt/lists/* \
    && userdel --remove ubuntu \
    && groupadd --gid 1000 subsyncd \
    && useradd --uid 1000 --gid 1000 --no-create-home --shell /usr/sbin/nologin subsyncd \
    && install -d -o 1000 -g 1000 -m 0750 /config /data /media
COPY --from=ffprobe-build /out/ffprobe /usr/local/bin/ffprobe
COPY --from=ffprobe-build /out/ffmpeg.cdx.json /usr/share/sbom/ffmpeg.cdx.json
COPY --from=ffprobe-build /out/COPYING.LGPLv2.1 /usr/share/licenses/ffprobe/COPYING.LGPLv2.1
COPY third_party/licenses/musl-COPYRIGHT /usr/share/licenses/ffprobe/musl-COPYRIGHT
COPY --from=go-build /out/subsyncd /usr/local/bin/subsyncd
COPY --from=lapse-release /out/lapse /opt/lapse
RUN chmod 0755 /usr/local/bin/subsyncd /opt/lapse/lapse \
    && install -d /usr/share/doc/subsyncd /usr/share/licenses/lapse \
    && cp /opt/lapse/LICENSE /usr/share/licenses/lapse/LICENSE
COPY config.example.yaml /usr/share/doc/subsyncd/config.example.yaml
ENV LD_LIBRARY_PATH=/opt/lapse
RUN ffprobe -version > /tmp/ffprobe-version \
    && grep -F "ffprobe version ${FFMPEG_VERSION} " /tmp/ffprobe-version \
    && /opt/lapse/lapse --vad > /tmp/lapse-vad \
    && grep -Fx silero /tmp/lapse-vad \
    && rm /tmp/ffprobe-version /tmp/lapse-vad
USER 1000:1000
EXPOSE 8097
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
  CMD ["curl", "-fsS", "http://127.0.0.1:8097/readyz"]
ENTRYPOINT ["subsyncd"]
CMD ["serve", "--config", "/config/config.yaml"]
