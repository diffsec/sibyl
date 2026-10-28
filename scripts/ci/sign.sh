#!/bin/bash
# Sign one image digest twice:
#  1. keyless, with the workflow identity, for anyone who checks with cosign;
#  2. with the project key, as an older ".sig" attachment, because podman and
#     bootc verify only key-based signatures in that format. The host's
#     /etc/containers/policy.json requires this one.
# The key signature is skipped when the digest already has one, so retagging
# an unchanged image does not add a second copy.
# Usage: sign.sh <image> <digest>. Needs COSIGN_PRIVATE_KEY and COSIGN_PASSWORD.
set -euo pipefail
image="$1"
digest="$2"
[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] || { echo "invalid digest: $digest" >&2; exit 1; }

if [[ "${SKIP_KEYLESS:-}" != 1 ]]; then
    cosign sign --yes "${image}@${digest}"
fi

sig_tag="${image}:${digest/:/-}.sig"
if docker buildx imagetools inspect "$sig_tag" >/dev/null 2>&1; then
    echo "key signature exists: $sig_tag"
else
    # --recursive signs the index and each manifest in it: podman checks the
    # platform image that it picks, not only the index.
    cosign sign --yes --recursive --key env://COSIGN_PRIVATE_KEY \
        --new-bundle-format=false --use-signing-config=false "${image}@${digest}"
fi
