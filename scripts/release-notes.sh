#!/bin/sh
# Prints the GitHub release notes for a release tag; see CONTRIBUTING.md.
# Usage: release-notes.sh TAG IMAGE DIGEST
set -eu

if [ "$#" -ne 3 ]; then
    echo "usage: $0 TAG IMAGE DIGEST" >&2
    exit 2
fi
tag=$1
image=$2
digest=$3

if ! printf '%s\n' "$tag" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'; then
    echo "release-notes: $tag is not a vX.Y.Z or vX.Y.Z-PRERELEASE tag" >&2
    exit 1
fi
git rev-parse -q --verify "refs/tags/$tag" >/dev/null || {
    echo "release-notes: tag $tag does not exist" >&2
    exit 1
}

repo_url="${GITHUB_SERVER_URL:-https://github.com}/${GITHUB_REPOSITORY:-tomislav/subsyncd}"
upgrade_url="$repo_url/blob/$tag/docs/upgrade-and-backup.md"

# Pre-releases are never a baseline: an rc and its final release both list
# everything since the previous stable release.
previous=$(git describe --tags --abbrev=0 --match 'v[0-9]*' --exclude '*-*' "$tag^" 2>/dev/null || true)

printf '## Image\n\n'
printf '`%s:%s` for linux/amd64 and linux/arm64, digest `%s`.\n\n' "$image" "${tag#v}" "$digest"

if [ -z "$previous" ]; then
    printf 'The first stable release. From here on, `latest` is the newest release, `edge` is unreleased `main`, and version numbers follow [Versioning](%s#versioning).\n\n' "$upgrade_url"
    printf '**Back up `data/` before upgrading** from an earlier build: this release may upgrade the database, and an older build cannot open the result. See [Upgrade](%s#upgrade).\n' "$upgrade_url"
    exit 0
fi

if [ -n "$(git diff --name-only --diff-filter=A "$previous" "$tag" -- internal/store/migrations/)" ]; then
    printf '**Back up `data/` before upgrading.** This release adds a database migration, and an older build cannot open the upgraded database. See [Upgrade](%s#upgrade).\n\n' "$upgrade_url"
fi

git log --no-merges --reverse --format=%s "$previous..$tag" | awk -v previous="$previous" '
match($0, /^[a-z]+(\([a-z0-9-]+\))?!?: /) {
    head = substr($0, 1, RLENGTH - 2)
    summary = substr($0, RLENGTH + 1)
    breaking = substr(head, length(head)) == "!"
    if (breaking) {
        head = substr(head, 1, length(head) - 1)
    }
    type = head
    scope = ""
    open = index(head, "(")
    if (open) {
        type = substr(head, 1, open - 1)
        scope = substr(head, open + 1, length(head) - open - 1) ": "
    }
    line = "- " scope summary "\n"
    if (breaking) {
        breaks = breaks line
    } else if (type == "feat") {
        features = features line
    } else if (type == "fix") {
        fixes = fixes line
    }
}
END {
    if (breaks != "") printf "## Breaking changes\n\n%s\n", breaks
    if (features != "") printf "## Features\n\n%s\n", features
    if (fixes != "") printf "## Fixes\n\n%s\n", fixes
    if (breaks == "" && features == "" && fixes == "") printf "No feature or fix commits since %s.\n\n", previous
}
'
printf 'Full list: %s/compare/%s...%s\n' "$repo_url" "$previous" "$tag"
