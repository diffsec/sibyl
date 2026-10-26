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

render() {
    sed -e "s|@PXE_SERVER_IP@|$PXE_SERVER_IP|g" \
        -e "s|@SIBYL_IMAGE@|$SIBYL_IMAGE|g" \
        -e "s|@INSTALL_DISK@|$INSTALL_DISK|g" \
        -e "s|@TIMEZONE@|$TIMEZONE|g" \
        -e "s|@SSH_KEY@|$SSH_KEY|g" "$1"
}
render /opt/pxe/grub.cfg.in > "$TFTP/uefi/grub.cfg"
chmod 0644 "$TFTP/uefi/grub.cfg"
render /opt/pxe/sibyl.ks.in > "$WEB/sibyl.ks"

echo "PXE ready. Router DHCP: next-server (option 66) = $PXE_SERVER_IP, boot file (option 67) = uefi/shimx64.efi"
httpd -f -p 80 -h "$WEB" &
exec dnsmasq --no-daemon --port=0 --enable-tftp --tftp-root="$TFTP" \
    --tftp-single-port --log-facility=-
