#!/usr/bin/env bash
# Build the GitHub release body for a tag, and print it to stdout.
#
# The body is the CHANGELOG.md section for the tag, wrapped in a short install
# note. goreleaser's own changelog is a list of commit subjects; the upgrade
# notes an operator needs live only in CHANGELOG.md. Copied from the kiosk.
#
# Usage:  scripts/release-notes.sh v0.4.0
#
# A FINAL tag requires its own `## [X.Y.Z]` section and fails without one, so a
# release cannot ship with an empty body because `## [Unreleased]` was not
# promoted. A PRERELEASE tag (v0.4.0-rc1) reads `## [Unreleased]` instead.
set -euo pipefail

TAG="${1:-}"
if [ -z "$TAG" ]; then
  echo "usage: $0 <tag>   e.g. $0 v0.5.0" >&2
  exit 2
fi

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHANGELOG="$ROOT/CHANGELOG.md"

VERSION="${TAG#v}"
case "$TAG" in
  *-*) HEADING='## [Unreleased]' ; KIND='prerelease' ;;
  *)   HEADING="## [$VERSION]"   ; KIND='release' ;;
esac

# Everything between this heading and the next `## ` heading, with surrounding
# blank lines trimmed. awk rather than sed: the section is hundreds of lines and
# contains `#` inside fenced code blocks, so the boundary has to be anchored to
# the start of a line and to the two-hash level exactly.
SECTION="$(
  awk -v want="$HEADING" '
    index($0, want) == 1 && substr($0, length(want) + 1, 1) ~ /^[ ]?$/ { grab = 1; next }
    grab && /^## / { exit }
    grab { print }
  ' "$CHANGELOG"
)"

# Trim leading and trailing blank lines.
SECTION="$(printf '%s\n' "$SECTION" | sed -e '/./,$!d' -e ':a' -e '/^\n*$/{$d;N;ba' -e '}')"

if [ -z "$(printf '%s' "$SECTION" | tr -d '[:space:]')" ]; then
  echo "release-notes: no content under '$HEADING' in CHANGELOG.md" >&2
  echo "release-notes: a $KIND tag needs that section filled in before tagging." >&2
  exit 1
fi

cat <<EOF
## services $TAG

Small, account-scoped NATS services for the Stone-Age.io platform. Each
service ships as a Linux binary archive below and as a container image:

- \`ghcr.io/stone-age-io/watchdog:$TAG\`
- \`ghcr.io/stone-age-io/creds-keeper:$TAG\`

---

$SECTION

---

Pre-1.0: treat a minor version as potentially breaking, and pin what you
deploy.

**Full changelog**: https://github.com/stone-age-io/services/commits/$TAG
EOF
