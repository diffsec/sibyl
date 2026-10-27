#!/bin/bash
# Install the Sibyl ISO into a VM and boot the installed disk.
#
# The VM has UEFI Secure Boot with Microsoft keys and a software TPM2, like
# the target host. A systemd credential starts sibyl-install with no
# questions. The test passes when the installed system boots without a
# passphrase, which proves the TPM2 unlock, and the admin account accepts SSH.
#
# Usage: tools/vmtest/install-test.sh <sibyl.iso> [work-dir]
# Needs: qemu-system-x86_64, qemu-img, swtpm, OVMF with Secure Boot, ssh.
# Uses KVM when /dev/kvm is usable, and is slow without it.
set -euo pipefail

iso="$(realpath "$1")"
work="${2:-$(mktemp -d)}"
mkdir -p "$work"
work="$(realpath "$work")"
install_timeout="${INSTALL_TIMEOUT:-2400}"
boot_timeout="${BOOT_TIMEOUT:-900}"

find_first() {
    local f
    for f in "$@"; do
        [[ -r "$f" ]] && { echo "$f"; return; }
    done
    return 1
}
code="${OVMF_CODE:-$(find_first /usr/share/edk2/ovmf/OVMF_CODE.secboot.fd /usr/share/OVMF/OVMF_CODE_4M.secboot.fd)}"
vars="${OVMF_VARS:-$(find_first /usr/share/edk2/ovmf/OVMF_VARS.secboot.fd /usr/share/OVMF/OVMF_VARS_4M.ms.fd)}"

accel=(-accel tcg)
if [[ -w /dev/kvm ]]; then
    accel=(-accel kvm -cpu host)
fi

log() { echo "vmtest: $*" >&2; }

cp "$vars" "$work/vars.fd"
qemu-img create -q -f qcow2 "$work/disk.qcow2" 80G
rm -f "$work/id_ed25519" "$work/id_ed25519.pub"
ssh-keygen -q -t ed25519 -N "" -C vmtest -f "$work/id_ed25519"

# The software TPM keeps its state in $work/tpm across both boots.
mkdir -p "$work/tpm"
swtpm socket --tpm2 --tpmstate "dir=$work/tpm" --ctrl "type=unixio,path=$work/swtpm.sock" \
    --daemon --pid "file=$work/swtpm.pid"
trap 'kill "$(cat "$work/swtpm.pid")" 2>/dev/null || true' EXIT

password="vmtest-$(head -c 12 /dev/urandom | base64 | tr -dc 'a-zA-Z0-9')"
cat > "$work/install.json" <<EOF
{"disk": "/dev/vda", "password": "$password",
 "admin": {"name": "admin", "keys": ["$(cat "$work/id_ed25519.pub")"]}}
EOF
cat > "$work/unit" <<'EOF'
[Unit]
Description=Sibyl unattended install test
After=multi-user.target

[Service]
Type=oneshot
ImportCredential=sibyl.install.json
Environment=SIBYL_INSTALL_DEBUG=1
StandardOutput=tty
StandardError=tty
TTYPath=/dev/ttyS0
ExecStart=/bin/sh -c 'if /usr/sbin/sibyl-install -config "$CREDENTIALS_DIRECTORY/sibyl.install.json"; then echo SIBYL-INSTALL-OK; else echo SIBYL-INSTALL-FAILED; findmnt; df -h; podman images; fi'
ExecStartPost=/usr/bin/systemctl poweroff
EOF
printf '[Unit]\nWants=sibyl-vmtest.service\n' > "$work/dropin"

cred() { echo "type=11,value=io.systemd.credential.binary:$1=$(base64 -w0 "$2")"; }

qemu() {
    qemu-system-x86_64 "${accel[@]}" -machine q35,smm=on -m 8192 -smp 4 \
        -global driver=cfi.pflash01,property=secure,value=on \
        -drive "if=pflash,format=raw,unit=0,readonly=on,file=$code" \
        -drive "if=pflash,format=raw,unit=1,file=$work/vars.fd" \
        -chardev "socket,id=chrtpm,path=$work/swtpm.sock" \
        -tpmdev emulator,id=tpm0,chardev=chrtpm -device tpm-crb,tpmdev=tpm0 \
        -drive "if=virtio,format=qcow2,file=$work/disk.qcow2" \
        -nic user,model=virtio-net-pci,hostfwd=tcp:127.0.0.1:2222-:22 \
        -display none -serial "file:$1" "${@:2}"
}

# wait_vm waits for a VM to exit, and kills it after $2 seconds.
wait_vm() {
    local pid=$1 deadline=$((SECONDS + $2))
    while kill -0 "$pid" 2>/dev/null; do
        if (( SECONDS > deadline )); then
            kill "$pid"
            return 1
        fi
        sleep 5
    done
}

log "installing from $iso (log: $work/install.log)"
qemu "$work/install.log" \
    -drive "media=cdrom,readonly=on,file=$iso" -boot order=d \
    -smbios "$(cred sibyl.install.json "$work/install.json")" \
    -smbios "$(cred systemd.extra-unit.sibyl-vmtest.service "$work/unit")" \
    -smbios "$(cred systemd.unit-dropin.multi-user.target~vmtest "$work/dropin")" &
wait_vm $! "$install_timeout" || log "the install VM timed out"
if ! grep -q SIBYL-INSTALL-OK "$work/install.log"; then
    tail -n 60 "$work/install.log" >&2
    log "FAIL: the install did not finish"
    exit 1
fi
grep -A1 -i "recovery key" "$work/install.log" | head -3 >&2 || true

log "booting the installed disk (log: $work/boot.log)"
qemu "$work/boot.log" -boot order=c &
vm=$!
ssh_cmd=(ssh -i "$work/id_ed25519" -p 2222 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null
    -o ConnectTimeout=5 -o BatchMode=yes admin@127.0.0.1)
deadline=$((SECONDS + boot_timeout))
until "${ssh_cmd[@]}" true 2>/dev/null; do
    if (( SECONDS > deadline )) || ! kill -0 "$vm" 2>/dev/null; then
        kill "$vm" 2>/dev/null || true
        tail -n 60 "$work/boot.log" >&2
        log "FAIL: no SSH login on the installed system"
        exit 1
    fi
    sleep 10
done
log "SSH login works; checking the installed system"
"${ssh_cmd[@]}" "cat > /tmp/check.sh" < "$(dirname "$0")/check-installed.sh"
if ! "${ssh_cmd[@]}" "echo '$password' | sudo -S -p '' bash /tmp/check.sh"; then
    kill "$vm"
    log "FAIL: the installed system checks"
    exit 1
fi
"${ssh_cmd[@]}" "echo '$password' | sudo -S -p '' systemctl poweroff" || true
wait "$vm" || true
log "PASS"
