#!/bin/bash
# Build an offline update bundle: one OCI archive per image, and a
# manifest.json with the SHA-256 of each. CI signs manifest.json with the
# project key (cosign sign-blob); sibyl-update checks that signature.
#
# The bundle holds the host image, every Sibyl service image for the same
# tag (every llama-swap backend of the variant, so the backend can change
# offline), and the third-party images that the Quadlet units pin by digest.
#
# Usage: scripts/bundle/make.sh <tag> <variant> <output directory>
# Example: scripts/bundle/make.sh sha-93a977a amd bundle
# Needs skopeo, python3 and sha256sum, and about 5 GB of free space.
set -euo pipefail
tag="$1"
variant="$2"
out="$3"
[[ "$tag" =~ ^[A-Za-z0-9._-]+$ && "$variant" =~ ^(amd|nvidia|intel)$ ]] || { echo "invalid tag or variant" >&2; exit 2; }
cd "$(dirname "$0")/../.."
mkdir -p "$out"
registry=ghcr.io/ihavespoons

# <store> <file> <reference>
images=(
    "os os.oci.tar $registry/sibyl-os:$tag-$variant"
    "sibyl sibyl-caddy.oci.tar $registry/sibyl-caddy:$tag"
    "sibyl sibyl-system-api.oci.tar $registry/sibyl-system-api:$tag"
    "tui sibyl-tui.oci.tar $registry/sibyl-tui:$tag"
)
# The llama-swap backends that each variant ships (hardware.Backends).
case "$variant" in
    amd) backends="hip vulkan" ;;
    nvidia) backends="cuda vulkan" ;;
    intel) backends="vulkan" ;;
esac
for b in $backends; do
    images+=("sibyl sibyl-llama-swap-$b.oci.tar $registry/sibyl-llama-swap:$tag-$b")
done
# Third-party images exactly as the Quadlet units pin them.
while read -r ref; do
    name="${ref%@*}"
    images+=("sibyl ${name##*/}.oci.tar $ref")
done < <(grep -h '^Image=.*@sha256:' os/rootfs/etc/containers/systemd/users/1001/*.container | cut -d= -f2- | sort -u)

: > "$out/.list"
for entry in "${images[@]}"; do
    read -r store file ref <<< "$entry"
    echo "==> $ref"
    # --preserve-digests keeps each manifest byte for byte, so the digests
    # that the Quadlet units pin still match after the host loads them. An
    # OCI archive cannot hold sigstore signatures; the signed manifest.json
    # carries the trust instead, with the SHA-256 of each archive.
    skopeo copy --quiet --preserve-digests --remove-signatures "docker://$ref" "oci-archive:$out/$file"
    printf '%s %s %s %s %s\n' "$store" "$file" "$ref" "$(sha256sum "$out/$file" | cut -d' ' -f1)" "$(stat -c %s "$out/$file")" >> "$out/.list"
done

python3 - "$out" "$tag" "$variant" <<'PY'
import datetime, json, sys
out, tag, variant = sys.argv[1:4]
images = []
for line in open(f"{out}/.list"):
    store, file, ref, sha, size = line.split()
    images.append({"ref": ref, "file": file, "sha256": sha, "size": int(size), "store": store})
manifest = {"version": 1, "tag": tag, "variant": variant,
            "created": datetime.datetime.now(datetime.UTC).strftime("%Y-%m-%dT%H:%M:%SZ"), "images": images}
with open(f"{out}/manifest.json", "w") as f:
    json.dump(manifest, f, indent=2)
    f.write("\n")
PY
rm -f "$out/.list"
echo "Wrote $out/manifest.json. Sign it: cosign sign-blob --key <key> --new-bundle-format=false --use-signing-config=false --output-signature $out/manifest.json.sig $out/manifest.json"
