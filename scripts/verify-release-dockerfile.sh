#!/bin/sh
set -eu

root_dockerfile=${1:-Dockerfile}
release_dockerfile=${2:-Dockerfile.release}
expected=$(mktemp)
trap 'rm -f "$expected"' EXIT

awk '
$0 == "RUN target_arch=\"${TARGETARCH:-$(go env GOARCH)}\"; \\" {
    print "RUN --mount=type=secret,id=opensubtitles_api_key,required=true \\"
    print "    target_arch=\"${TARGETARCH:-$(go env GOARCH)}\"; \\"
    print "    opensubtitles_api_key=\"$(cat /run/secrets/opensubtitles_api_key)\"; \\"
    print "    case \"${opensubtitles_api_key}\" in \\"
    print "      \"\"|*[!A-Za-z0-9._-]*) echo \"OpenSubtitles API key is empty or contains unsupported characters\" >&2; exit 1 ;; \\"
    print "    esac; \\"
    print "    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${target_arch} \\"
    print "    go build -trimpath -ldflags=\"-s -w -X subsyncd/internal/version.Value=${VERSION} -X subsyncd/internal/provider/opensubtitles.builtInAPIKey=${opensubtitles_api_key}\" -o /out/subsyncd ./cmd/subsyncd"

    if ((getline) <= 0 || $0 != "    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${target_arch} \\") {
        exit 2
    }
    if ((getline) <= 0 || $0 != "    go build -trimpath -ldflags=\"-s -w -X subsyncd/internal/version.Value=${VERSION}\" -o /out/subsyncd ./cmd/subsyncd") {
        exit 2
    }
    replacements++
    next
}
{ print }
END {
    if (replacements != 1) {
        exit 2
    }
}
' "$root_dockerfile" >"$expected"

if ! cmp -s "$expected" "$release_dockerfile"; then
    echo "Dockerfile.release differs from the permitted secret-linking transformation" >&2
    diff -u "$expected" "$release_dockerfile" >&2 || true
    exit 1
fi
