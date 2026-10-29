#!/bin/bash
# Pin os/Containerfile to the current digest of quay.io/fedora/fedora-bootc:<release>.
# Quay deletes old Fedora manifests after the tag moves, so a stale pin breaks
# the build within days. Run this, review the diff, and commit.
# Usage: scripts/bump-base-image.sh [release]
set -euo pipefail
release="${1:-44}"
cd "$(git rev-parse --show-toplevel)"
digest="$(curl -fsS "https://quay.io/api/v1/repository/fedora/fedora-bootc/tag/?specificTag=${release}&onlyActiveTags=true" \
    | python3 -c 'import sys, json; print(json.load(sys.stdin)["tags"][0]["manifest_digest"])')"
[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "unexpected digest: $digest" >&2; exit 1; }
sed -i.bak -E "s#^(FROM quay.io/fedora/fedora-bootc:)[0-9]+@sha256:[0-9a-f]{64}#\1${release}@${digest}#" os/Containerfile
rm -f os/Containerfile.bak
grep -n '^FROM quay.io/fedora/fedora-bootc:' os/Containerfile
