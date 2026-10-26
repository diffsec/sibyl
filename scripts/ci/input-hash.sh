#!/bin/bash
# Print a hash of every tracked file under the given paths.
# It uses git blob IDs, so it changes only when a file's content, mode or
# path changes. CI tags each image with this hash and skips the build when
# an image with the same hash already exists.
# Usage: input-hash.sh <path>...
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
git ls-files --stage -- "$@" | sha256sum | cut -c1-16
