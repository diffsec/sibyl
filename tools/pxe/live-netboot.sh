#!/bin/bash
# Prepare a network boot of the Sibyl live system that installs with no
# questions. It downloads the ISO, unpacks the kernel, initrd and squashfs
# into www/live, and writes www/live/grub.cfg. The install settings travel as
# one systemd credential on the kernel command line, which the live image's
# sibyl-autoinstall.service reads.
#
# Warning: the default menu entry erases INSTALL_DISK with no confirmation.
#
# Needs tools/pxe/.env with PXE_SERVER_IP, SSH_KEY and ADMIN_PASSWORD, and
# docker, bsdtar and base64 on this host. Usage: tools/pxe/live-netboot.sh
set -euo pipefail
cd "$(dirname "$0")"
# .env is in docker compose format: KEY=value, with spaces and no quotes.
while IFS='=' read -r key value; do
    [[ "$key" =~ ^[A-Z_][A-Z0-9_]*$ ]] && [[ -z "${!key:-}" ]] && export "$key=$value"
done < .env
: "${PXE_SERVER_IP:?set PXE_SERVER_IP in .env}"
: "${SSH_KEY:?set SSH_KEY in .env}"
: "${ADMIN_PASSWORD:?set ADMIN_PASSWORD in .env}"
ISO_REF="${ISO_REF:-ghcr.io/diffsec/sibyl-iso:latest-amd}"
INSTALL_DISK="${INSTALL_DISK:-nvme0n1}"
ADMIN_NAME="${ADMIN_NAME:-admin}"
ORAS_IMAGE="ghcr.io/oras-project/oras:v1.3.6@sha256:9cfb578be975b762fd898b9d004243d8003aeb807a3e15b2ed212cdd1326c9eb"

mkdir -p www/iso www/live
# ISO_FILE uses a local ISO, for example one built on a Linux host with
# image-builder, instead of pulling one from GHCR.
if [[ -n "${ISO_FILE:-}" ]]; then
    iso="$ISO_FILE"
else
    echo "Pulling $ISO_REF"
    docker run --rm -v "$PWD/www/iso:/work" -w /work "$ORAS_IMAGE" pull "$ISO_REF"
    iso="$(find www/iso -maxdepth 1 -name "*.iso" -print -quit)"
    (cd www/iso && shasum -a 256 -c SHA256SUMS)
fi

echo "Unpacking $iso"
bsdtar -xf "$iso" -C www/live --strip-components 2 images/pxeboot/vmlinuz images/pxeboot/initrd.img
bsdtar -xf "$iso" -C www/live --strip-components 1 LiveOS/squashfs.img

# The password goes over the LAN only as a SHA-512 crypt hash.
hash="$(printf '%s\n' "$ADMIN_PASSWORD" | docker run --rm -i docker.io/library/alpine:3.22@sha256:2e0b81d60597f449baa56dc25d355367869d2dca4cd57e866f1e2b52af3eb55f \
    sh -c 'apk add -q openssl >/dev/null && openssl passwd -6 -stdin')"
config="$(printf '{"disk":"/dev/%s","then":"reboot","admin":{"name":"%s","password_hash":"%s","keys":["%s"]}}' \
    "$INSTALL_DISK" "$ADMIN_NAME" "$hash" "$SSH_KEY")"
cred="$(printf '%s' "$config" | base64 | tr -d '\n')"
args="systemd.set_credential_binary=sibyl.install.json:$cred"

# DEBUG_SSH=1 lets SSH_KEY log in as root to the live system, so a failed
# unattended install can be inspected over the network. It changes only the
# netbooted live system in RAM, never the installed one.
if [[ "${DEBUG_SSH:-}" == 1 ]]; then
    key="$(printf '%s\n' "$SSH_KEY" | base64 | tr -d '\n')"
    # systemd expands $OPTIONS from sshd's environment file, not this shell.
    dropin="$(printf '%s\n' '[Service]' 'ExecStart=' \
        "ExecStart=/usr/sbin/sshd -D -o PermitRootLogin=prohibit-password \$OPTIONS" | base64 | tr -d '\n')"
    args="$args systemd.set_credential_binary=ssh.authorized_keys.root:$key"
    args="$args systemd.set_credential_binary=systemd.unit-dropin.sshd.service~debug:$dropin"
fi

cat > www/live/grub.cfg <<EOF
# Written by tools/pxe/live-netboot.sh. GRUB loads the kernel and initrd over
# HTTP, and dracut's livenet loads the squashfs over HTTP.
set default=0
set timeout=10

menuentry 'Install Sibyl, no questions (erases $INSTALL_DISK)' --id install {
    linux (http,$PXE_SERVER_IP)/live/vmlinuz root=live:http://$PXE_SERVER_IP/live/squashfs.img rd.live.image rd.live.overlay.overlayfs=1 rd.neednet=1 ip=dhcp enforcing=0 $args
    initrd (http,$PXE_SERVER_IP)/live/initrd.img
}

menuentry 'Sibyl live system' --id live {
    linux (http,$PXE_SERVER_IP)/live/vmlinuz root=live:http://$PXE_SERVER_IP/live/squashfs.img rd.live.image rd.live.overlay.overlayfs=1 rd.neednet=1 ip=dhcp enforcing=0
    initrd (http,$PXE_SERVER_IP)/live/initrd.img
}

menuentry 'Boot from local disk' --id local {
    exit
}
EOF
echo "Kernel command line: $(grep -m1 'linux ' www/live/grub.cfg | wc -c) bytes (the limit is 2048)"
echo "Ready. Start the TFTP server with PXE_MODE=live, and serve www on port 80:"
echo "  PXE_MODE=live docker compose up --build -d"
echo "  (cd www && python3 -c 'import http.server as h; h.SimpleHTTPRequestHandler.protocol_version = \"HTTP/1.1\"; h.ThreadingHTTPServer((\"0.0.0.0\", 80), h.SimpleHTTPRequestHandler).serve_forever()')"
