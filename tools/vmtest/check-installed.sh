#!/bin/bash
# Checks that install-test.sh runs as root on the installed system.
set -euo pipefail
echo "cmdline: $(cat /proc/cmdline)"
mokutil --sb-state | grep -q "SecureBoot enabled"
mapper="$(findmnt -n -o SOURCE /sysroot)"
root="$(cryptsetup status "$mapper" | awk '/device:/ {print $2}')"
slots="$(systemd-cryptenroll "$root")"
echo "$slots"
grep -q tpm2 <<< "$slots"
grep -q recovery <<< "$slots"
if grep -q password <<< "$slots"; then
    echo "the installer key slot is still there" >&2
    exit 1
fi
id admin | grep -q wheel
[[ "$(getenforce)" == Enforcing ]]
bootc status --format=humanreadable | head -5
echo "installed system: all checks pass"
