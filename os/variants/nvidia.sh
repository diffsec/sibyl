#!/bin/bash
# NVIDIA variant: the driver's open kernel modules, built for the image's
# kernel and signed with the project MOK key, plus the NVIDIA Container
# Toolkit for CDI. Runs once in os/Containerfile.
#
# The MOK private key comes from the build secret "mok_key". Without it the
# modules are unsigned, and Secure Boot refuses to load them. Each host
# enrolls /usr/share/sibyl/sibyl-mok.der once: sudo mokutil --import ...
set -euo pipefail
kver="$(ls /usr/lib/modules)"
fedora="$(rpm -E %fedora)"

dnf -y install --setopt=install_weak_deps=False \
    "https://mirrors.rpmfusion.org/free/fedora/rpmfusion-free-release-${fedora}.noarch.rpm" \
    "https://mirrors.rpmfusion.org/nonfree/fedora/rpmfusion-nonfree-release-${fedora}.noarch.rpm"
curl -fsSL https://nvidia.github.io/libnvidia-container/stable/rpm/nvidia-container-toolkit.repo \
    -o /etc/yum.repos.d/nvidia-container-toolkit.repo

# Build the open kernel modules, which NVIDIA recommends for Turing and newer.
echo '%_with_kmod_nvidia_open 1' > /etc/rpm/macros.nvidia-kmod

dnf -y install --setopt=install_weak_deps=False \
    "kernel-devel-${kver}" akmods akmod-nvidia xorg-x11-drv-nvidia-cuda nvidia-container-toolkit

# akmods signs the modules it builds when these two files exist. Its build
# runs as the akmods user, which the akmods package creates, so the key
# belongs to that group.
install -d -m 0750 -g akmods /etc/pki/akmods/private
install -d -m 0755 /etc/pki/akmods/certs
install -m 0644 /variants/sibyl-mok.der /etc/pki/akmods/certs/public_key.der
if [[ -s /run/secrets/mok_key ]]; then
    install -m 0640 -g akmods /run/secrets/mok_key /etc/pki/akmods/private/private_key.priv
else
    echo "nvidia.sh: no mok_key build secret: the NVIDIA modules are unsigned" >&2
fi
# akmods can exit 0 after a failed build, so modinfo decides.
akmods --force --kernels "${kver}" --kmod nvidia || true
if ! modinfo -k "${kver}" nvidia | grep -E '^(version|signer|sig_key):'; then
    echo "nvidia.sh: the nvidia module was not built" >&2
    tail -n 80 /var/cache/akmods/nvidia/*.failed.log >&2 || true
    exit 1
fi

# Keep only the public certificate, for mokutil.
rm -rf /etc/pki/akmods/private
install -D -m 0644 /variants/sibyl-mok.der /usr/share/sibyl/sibyl-mok.der

# CDI devices for podman: nvidia-cdi-refresh writes them at boot and after
# driver changes, as nvidia.com/gpu=all and per-GPU names.
systemctl enable nvidia-cdi-refresh.path nvidia-cdi-refresh.service

# The packages above add files to /var that bootc does not want in an image.
dnf clean all
rm -rf /var/cache/* /var/log/* /var/lib/dnf /var/tmp/* /run/akmods /run/dnf
