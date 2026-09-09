# Minimal ffprobe packaging experiment — 2026-09-09

## Result

The local arm64 experiment replaces Debian's general-purpose ffmpeg package with a static FFmpeg 8.1 ffprobe executable. It retains Debian 13.2, the existing Go application and the complete checksummed LAPSE v2.0.5 release bundle. Production Dockerfiles and publication are unchanged.

| Measurement | Baseline | Minimal ffprobe |
| --- | ---: | ---: |
| Sum of gzip-compressed image layers | 219,077,155 bytes | 60,142,143 bytes |
| Sum of uncompressed layer tar streams | 574,690,816 bytes | 175,719,424 bytes |
| Standalone static ffprobe executable | Distribution executable uses shared libraries | 1,905,136 bytes |

This saves 72.5% of compressed layer bytes and 69.4% of uncompressed layer tar bytes. These measurements come from the actual gzip layer blobs exported by Docker, not gzip of an already compressed image archive. They exclude image manifests/configuration metadata and are not measurements of resident memory or exact unpacked filesystem allocation. The native build used the same application binary in both images to isolate packaging effects.

Local image names: `subsyncd:ffprobe-baseline`, `subsyncd:ffprobe-minimal`, and the tool-only `subsyncd:ffprobe-tool`. Work files and raw results are under `/tmp/subsyncd-ffprobe-spike`; those files are disposable, not deployment inputs.

## Runtime dependencies

The baseline installs `ca-certificates curl ffmpeg libfftw3-double3 tzdata`. The experimental runtime replaces `ffmpeg` with the static probe and explicitly installs `libstdc++6`, which was previously supplied transitively. ONNX Runtime needs that C++ library. The experiment otherwise retains the same UID/GID, health command and LAPSE location/environment.

`ldd` found every LAPSE/ONNX dependency in the smaller image; LAPSE reports the Silero backend. The LAPSE executable, ONNX library and Silero model have identical SHA-256 checksums between the images. The probe's only compiled protocol is `file`.

## Validation and limits

- Eight synthetic media fixtures produced identical inventory fields in the distribution and static probes: stream index/type/codec, language/title, and default/forced/HI dispositions.
- The amd64 static probe also passed all eight comparisons and the corrupt-input check under QEMU. Its ELF architecture was verified and its executable size is 1,966,320 bytes. The local legacy Docker cross-platform build failed to preserve platform metadata, and GCC crashed under emulation during a configure check. Building inside an explicitly amd64 container with Clang/LLD succeeded using the same feature allowlist (`--cc=clang --extra-ldflags="-static -fuse-ld=lld"`). Complete image-size and startup/LAPSE measurements above are arm64 only.
- Fixtures cover H.264/AAC/SRT in MKV, H.264/AAC/ASS in MKV, H.264/AAC/MOV text in MP4, VP9/Opus/WebVTT in WebM, MPEG-4/MP3 in AVI, H.264/AAC in MPEG-TS, HEVC/AC3 in MKV, and AV1/FLAC in MKV.
- Both probes reject a corrupt MKV. Normal fixtures include both explicit forced disposition and an unflagged title-marked forced track.
- Both runtime images pass startup, readiness and `doctor` with networking disabled, a read-only root filesystem and private writable `/tmp` data/media directories.
- Real LAPSE processing of a 300-second synthetic tone track with 40 subtitle cues reaches the expected typed no-speech condition in both images. This checks audio decoding and VAD loading, not successful alignment against human speech.
- Production sample results are recorded separately below. The operator file was read before access; its host-specific details are intentionally excluded from this document. Real speech alignment and media-library-wide compatibility remain outside the measured coverage.

## Read-only production sample

With explicit user approval, compared the static arm64 probe against the distribution probe in the already available `sha-8532a69` image. The configured service had no container, so the check used a separate diagnostic container with networking disabled, a read-only root filesystem, one CPU, 256 MB memory, dropped capabilities and no privilege escalation. Exactly 12 selected files were individually bind-mounted read-only. Media symlinks were resolved only inside the operator-documented media roots.

All 12 files returned identical inventory fields. The sample contained HEVC video, E-AC-3 audio, SRT and PGS subtitles, 230 subtitle tracks in total, and one file with no subtitle tracks. This extends bitmap-track coverage to PGS; it does not establish VobSub/DVB or every container's compatibility. No application initialization, database operation, provider request, synchronization or subtitle mutation ran against production media. No media contents or filenames were copied into this report. The temporary executable and diagnostic container were removed after the test.

Per-file timings were recorded for diagnostics but are not a fair performance benchmark: the baseline ran first, so the second probe could benefit from filesystem cache warming.

## Reproducing the native tool build

Download the official source archive into an isolated build context as `ffmpeg.tar.xz`:

```sh
curl -fL https://ffmpeg.org/releases/ffmpeg-8.1.tar.xz -o ffmpeg.tar.xz
```

Use this experimental Dockerfile; the archive digest is checked before extraction:

```dockerfile
FROM alpine:3.24 AS build
RUN apk add --no-cache build-base nasm linux-headers
COPY ffmpeg.tar.xz /tmp/ffmpeg.tar.xz
RUN echo 'b072aed6871998cce9b36e7774033105ca29e33632be5b6347f3206898e0756a  /tmp/ffmpeg.tar.xz' | sha256sum -c - && tar xf /tmp/ffmpeg.tar.xz -C /tmp
WORKDIR /tmp/ffmpeg-8.1
RUN ./configure \
    --disable-everything --disable-autodetect --disable-doc --disable-debug \
    --disable-network --disable-avdevice --disable-avfilter \
    --disable-swscale --disable-swresample --disable-programs \
    --enable-ffprobe --enable-small --enable-static --disable-shared \
    --extra-ldflags=-static --enable-protocol=file \
    --enable-demuxer=matroska,mov,avi,mpegts,mpegps,ogg,flv,asf \
    --enable-parser=h264,hevc,av1,vp9,mpeg4video,mpegvideo,aac,aac_latm,ac3,mpegaudio,flac,opus,vorbis,dca \
    --enable-decoder=subrip,ass,ssa,mov_text,webvtt,pgssub,dvdsub,dvbsub \
    && make -j4 ffprobe && strip ffprobe
FROM scratch
COPY --from=build /tmp/ffmpeg-8.1/ffprobe /ffprobe
ENTRYPOINT ["/ffprobe"]
```

For the comparison runtime, copy `/ffprobe` to `/usr/local/bin/ffprobe`, remove `ffmpeg` from the package list, add `libstdc++6`, and copy the same application and LAPSE bundle from the local baseline image. Do not derive the smaller runtime from the baseline final image: deleting installed packages in a new layer would retain their bytes in the parent layers.

Before adopting this in release builds, pin the builder image digest, inventory the source-built FFmpeg component in the SBOM, validate the complete image on native amd64 as well as arm64, and broaden the corpus to the remaining bitmap formats and successful real-speech alignment. Keep root `Dockerfile` and `Dockerfile.release` synchronized. The measured savings justify that follow-up; the experiment remains local.
