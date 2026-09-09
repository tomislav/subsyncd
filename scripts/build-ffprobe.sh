#!/bin/sh
set -eu

version=${1:?FFmpeg version required}
source_sha256=${2:?FFmpeg source SHA-256 required}
target_arch=${3:-}
case "$(uname -m)" in
    x86_64) native_arch=amd64 ;;
    aarch64) native_arch=arm64 ;;
    *) echo 'unsupported ffprobe architecture' >&2; exit 1 ;;
esac
if [ "${target_arch:-$native_arch}" != "$native_arch" ]; then
    echo 'ffprobe build platform does not match TARGETARCH' >&2
    exit 1
fi

mkdir -p /build /out
source_url="https://ffmpeg.org/releases/ffmpeg-${version}.tar.xz"
curl -fsSL --retry 3 -o /build/ffmpeg.tar.xz "$source_url"
printf '%s  /build/ffmpeg.tar.xz\n' "$source_sha256" | sha256sum -c -
tar -xf /build/ffmpeg.tar.xz -C /build
cd "/build/ffmpeg-${version}"

# Local inventory inspection only: no encoders, networking, preview or devices.
./configure \
    --disable-everything --disable-autodetect --disable-doc --disable-debug \
    --disable-network --disable-avdevice --disable-avfilter \
    --disable-swscale --disable-swresample --disable-programs \
    --enable-ffprobe --enable-small --enable-static --disable-shared \
    --cc=clang --extra-ldflags="-static -fuse-ld=lld" \
    --enable-protocol=file \
    --enable-demuxer=matroska,mov,avi,mpegts,mpegps,ogg,flv,asf \
    --enable-parser=h264,hevc,av1,vp9,mpeg4video,mpegvideo,aac,aac_latm,ac3,mpegaudio,flac,opus,vorbis,dca \
    --enable-decoder=subrip,ass,ssa,movtext,webvtt,pgssub,dvdsub,dvbsub
make -j"$(getconf _NPROCESSORS_ONLN)" ffprobe
strip ffprobe
cp ffprobe /out/ffprobe
cp COPYING.LGPLv2.1 /out/COPYING.LGPLv2.1

# Source-built components need explicit inventory; there is no runtime package
# database entry for this executable. Keep provenance tied to the verified input.
musl_version=$(awk 'BEGIN { RS=""; FS="\n" } /(^|\n)P:musl\n/ { for (i=1; i<=NF; i++) if ($i ~ /^V:/) print substr($i,3) }' /lib/apk/db/installed)
[ -n "$musl_version" ]
cat > /out/ffmpeg.cdx.json <<EOF
{
  "bomFormat": "CycloneDX",
  "specVersion": "1.5",
  "version": 1,
  "components": [{
    "type": "application",
    "name": "ffmpeg",
    "version": "${version}",
    "purl": "pkg:generic/ffmpeg@${version}",
    "cpe": "cpe:2.3:a:ffmpeg:ffmpeg:${version}:*:*:*:*:*:*:*",
    "externalReferences": [{
      "type": "distribution",
      "url": "${source_url}",
      "hashes": [{"alg": "SHA-256", "content": "${source_sha256}"}]
    }]
  }, {
    "type": "library",
    "name": "musl",
    "version": "${musl_version}",
    "purl": "pkg:apk/alpine/musl@${musl_version}",
    "licenses": [{"license": {"id": "MIT"}}]
  }]
}
EOF
