#!/bin/sh
# Download and verify the Fedora installer files, render the GRUB menu and the
# kickstart, then serve TFTP (UDP 69, single port) and HTTP (TCP 80).
set -eu

: "${PXE_SERVER_IP:?set PXE_SERVER_IP to the LAN address of this host}"
: "${SSH_KEY:?set SSH_KEY to the public key for the admin user}"
FEDORA_RELEASE="${FEDORA_RELEASE:-44}"
FEDORA_MIRROR="${FEDORA_MIRROR:-https://dl.fedoraproject.org/pub/fedora/linux/releases}"
SIBYL_IMAGE="${SIBYL_IMAGE:-ghcr.io/ihavespoons/sibyl-os:latest-amd}"
INSTALL_DISK="${INSTALL_DISK:-nvme0n1}"
TIMEZONE="${TIMEZONE:-Etc/UTC}"
# GRUB_KERNEL_VIA=http is fast (seconds) but needs a direct path to the HTTP
# server. GRUB's small TCP stack fails through the Docker port forwarder on
# macOS. tftp works everywhere but takes about 14 minutes for the initrd.
GRUB_KERNEL_VIA="${GRUB_KERNEL_VIA:-http}"
case "$GRUB_KERNEL_VIA" in
    http) BOOT_SOURCE="(http,$PXE_SERVER_IP)" ;;
    tftp) BOOT_SOURCE="" ;;
    *) echo "GRUB_KERNEL_VIA must be http or tftp" >&2; exit 1 ;;
esac

# PXE_MODE=live serves only shim, GRUB and the menu that live-netboot.sh
# wrote. A native HTTP server on the host serves the live files.
if [ "${PXE_MODE:-}" = live ]; then
    [ -f /www/live/grub.cfg ] || { echo "run tools/pxe/live-netboot.sh first" >&2; exit 1; }
    TFTP=/data/srv/tftp
    mkdir -p "$TFTP/uefi"
    cp /opt/pxe/uefi/shimx64.efi /opt/pxe/uefi/grubx64.efi /www/live/grub.cfg "$TFTP/uefi/"
    cp "$TFTP/uefi/grubx64.efi" "$TFTP/uefi/grub.cfg" "$TFTP/"
    chmod 0644 "$TFTP/uefi/"* "$TFTP/grubx64.efi" "$TFTP/grub.cfg"
    echo "PXE live mode ready on $PXE_SERVER_IP"
    exec dnsmasq --no-daemon --port=0 --enable-tftp --tftp-root="$TFTP" \
        --tftp-single-port --log-facility=-
fi

TREE="$FEDORA_MIRROR/$FEDORA_RELEASE/Everything/x86_64/os"
# The root filesystem is read-only. Rendered files and downloads both go to
# the /data volume.
WEB=/data/srv/http
TFTP=/data/srv/tftp
DATA="/data/fedora$FEDORA_RELEASE"
mkdir -p "$DATA/images/pxeboot" "$WEB" "$TFTP/uefi"
cp /opt/pxe/uefi/shimx64.efi /opt/pxe/uefi/grubx64.efi "$TFTP/uefi/"
chmod 0644 "$TFTP/uefi/"*

curl -fsSL "$TREE/.treeinfo" -o "$DATA/.treeinfo"
for f in images/pxeboot/vmlinuz images/pxeboot/initrd.img images/install.img; do
    want="$(sed -n "s|^$f = sha256:||p" "$DATA/.treeinfo")"
    [ -n "$want" ] || { echo "no checksum for $f in .treeinfo" >&2; exit 1; }
    if [ -f "$DATA/$f" ] && echo "$want  $DATA/$f" | sha256sum -c -s; then
        echo "cached: $f"
        continue
    fi
    echo "downloading: $f"
    curl -fL --retry 3 -o "$DATA/$f.part" "$TREE/$f"
    echo "$want  $DATA/$f.part" | sha256sum -c -s || { echo "checksum mismatch: $f" >&2; exit 1; }
    mv "$DATA/$f.part" "$DATA/$f"
done
ln -sfn "$DATA" "$WEB/fedora"
ln -sfn "$DATA" "$TFTP/fedora"

render() {
    sed -e "s|@PXE_SERVER_IP@|$PXE_SERVER_IP|g" \
        -e "s|@SIBYL_IMAGE@|$SIBYL_IMAGE|g" \
        -e "s|@INSTALL_DISK@|$INSTALL_DISK|g" \
        -e "s|@TIMEZONE@|$TIMEZONE|g" \
        -e "s|@BOOT_SOURCE@|$BOOT_SOURCE|g" \
        -e "s|@SSH_KEY@|$SSH_KEY|g" "$1"
}
render /opt/pxe/grub.cfg.in > "$TFTP/uefi/grub.cfg"
chmod 0644 "$TFTP/uefi/grub.cfg"
# shim asks for grubx64.efi at the TFTP root, not next to itself, and GRUB
# then reads grub.cfg from the directory it was loaded from.
cp "$TFTP/uefi/grubx64.efi" "$TFTP/uefi/grub.cfg" "$TFTP/"
render /opt/pxe/sibyl.ks.in > "$WEB/sibyl.ks"

echo "PXE ready. Router DHCP: next-server (option 66) = $PXE_SERVER_IP, boot file (option 67) = uefi/shimx64.efi"
httpd -f -v -p 80 -h "$WEB" &
exec dnsmasq --no-daemon --port=0 --enable-tftp --tftp-root="$TFTP" \
    --tftp-single-port --log-facility=-
