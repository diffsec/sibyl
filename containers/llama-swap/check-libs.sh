#!/bin/sh
# Exit non-zero if any ELF file under the given directories has a shared
# library dependency that the dynamic loader cannot find.
# Usage: check-libs.sh <dir>...
set -eu
missing="$(find "$@" -type f \( -perm -u+x -o -name '*.so*' \) -exec ldd {} \; 2>/dev/null | grep 'not found' | sort -u || true)"
if [ -n "$missing" ]; then
    echo "unresolved shared libraries:" >&2
    echo "$missing" >&2
    exit 1
fi
echo "all shared libraries resolve"
