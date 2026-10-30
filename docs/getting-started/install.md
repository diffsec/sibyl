# Install Sibyl

This page takes you from a computer with a GPU to an installed Sibyl host. You download the installer, write it to a USB stick, change the firmware settings, and install. After the install, continue with [First boot and setup](first-boot.md).

You must be able to change firmware settings and use a terminal. You do not need to know Linux well.

Sibyl is pre-alpha. The AMD variant is tested on one host with two Radeon RX 7900 XTX cards. The NVIDIA and Intel variants build and pass the same checks, but no one has run them on real hardware yet.

## What you need

| Item | Requirement |
|---|---|
| Computer | x86-64 with UEFI firmware. Secure Boot is recommended. |
| TPM | A TPM 2.0 chip, or the firmware TPM (AMD fTPM or Intel PTT). Without one, you type a passphrase at every boot. |
| GPU | One or more GPUs from one vendor. See the table below. |
| Disk | One internal disk of 64 GiB or more. The installer erases it. Models need more space, so plan for the models you want. |
| USB stick | 8 GB or more for the installer. 32 GB or more if you want to run Sibyl from the stick. |
| Network | Wired Ethernet with DHCP and internet access. The first boot downloads the service images, a few GB. |
| Second computer | To download the installer, write the USB stick, and later use Sibyl from a browser. |

Each GPU vendor has its own Sibyl image, called a variant. Pick the variant that matches your GPUs.

| Variant | GPUs | Backends |
|---|---|---|
| `amd` | Radeon and Ryzen AI. HIP runs on RX 6800 and 6900, RX 7000, RX 9000, Radeon PRO W6800, W7000 and AI PRO R9700, and Ryzen AI 300 and Strix Halo APUs. Other Radeon cards use Vulkan. | HIP (ROCm), Vulkan |
| `nvidia` | GeForce RTX 20 series and newer, and data-center GPUs from Turing on | CUDA, Vulkan |
| `intel` | Arc and recent integrated graphics | Vulkan |

A backend is the GPU code that llama.cpp uses. Sibyl can measure which backend is faster on your hardware. See [Models](../guides/models.md) for the benchmark. The [README](../../README.md) lists the test status of each variant.

## Get the installer

The installer is an ISO file. It is published as an OCI artifact on GitHub's container registry, because it is too large for a GitHub release. You check its signature, download it, and write it to the USB stick.

1. Install `oras` and `cosign` on the second computer. See [oras.land](https://oras.land/docs/installation) and [docs.sigstore.dev](https://docs.sigstore.dev/cosign/system_config/installation/).
2. Find the ISO for your variant, and check its signature with the Sibyl public key. `cosign` must print `The signatures were verified against the specified public key`. This example is for `amd`:

   ```bash
   DIGEST="$(oras resolve ghcr.io/diffsec/sibyl-iso:latest-amd)"
   cosign verify --new-bundle-format=false \
     --key https://raw.githubusercontent.com/diffsec/sibyl/main/os/rootfs/etc/pki/containers/sibyl-cosign.pub \
     "ghcr.io/diffsec/sibyl-iso@$DIGEST"
   ```

3. Download that ISO. `oras` checks each file against the digest that the signature covers:

   ```bash
   mkdir sibyl-iso && cd sibyl-iso
   oras pull "ghcr.io/diffsec/sibyl-iso@$DIGEST"
   ```

4. Make sure that the file is complete. The command must print `OK`. On Linux, use `sha256sum -c SHA256SUMS` instead:

   ```bash
   shasum -a 256 -c SHA256SUMS
   ```

5. Write the ISO to the USB stick. This erases the stick. See [Write the USB stick](#write-the-usb-stick).

The key signature uses the older cosign format, because podman and bootc read only that format. cosign 3 needs `--new-bundle-format=false` to find it, and prints that the flag is deprecated. ISOs built before 2026-10-29 have no key signature.

The ISO file name contains the build tag, such as `sibyl-sha-339b7a4-amd.iso`. The examples below write it as `sibyl-*.iso`.

### Write the USB stick

Writing the ISO erases everything on the stick. Make sure that you select the stick and not a disk that you need.

On Linux:

1. Find the stick with `lsblk`.
2. Write the ISO. Replace `/dev/sdX` with the stick:

   ```bash
   sudo dd if=sibyl-*.iso of=/dev/sdX bs=4M conv=fsync status=progress
   ```

On macOS:

1. Find the stick with `diskutil list`.
2. Unmount the stick and write the ISO. Replace `N` with the number of the stick:

   ```bash
   diskutil unmountDisk /dev/diskN
   sudo dd if=sibyl-*.iso of=/dev/rdiskN bs=4m
   ```

On Windows, use [balenaEtcher](https://etcher.balena.io/), or Rufus in DD image mode.

## Prepare the computer

These firmware settings let the TPM protect the disk and let the computer boot from the stick. The key to open the firmware setup is usually Del, F2 or F10 at power-on.

1. Turn on Secure Boot. If the firmware is in Setup Mode, install the default keys first.
2. Turn on the TPM. It is often called fTPM, PTT or "Security Device Support".
3. Set the boot order to USB first, or note the key for the one-time boot menu. This key is often F8, F11 or F12.
4. Set a firmware password, and lock the boot order.

The TPM unlocks the disk for any system that boots through the same signed boot loader. A locked firmware is part of the protection.

If you use the `nvidia` variant, the NVIDIA driver is signed with the Sibyl project key. Secure Boot loads the driver only after you enroll that key. You do this after the install, in [NVIDIA key enrollment](first-boot.md#nvidia-key-enrollment).

## Install to the disk

The installer erases one internal disk, encrypts it, and installs Sibyl on it.

1. Plug in the USB stick and the network cable.
2. Boot from the stick. The live system starts and shows a menu:

   ```
     1  Install Sibyl to a disk
     2  Make a persistence partition on this USB
     3  Setup wizard (needs persistence)
     4  Shell
     5  Reboot
     6  Power off
   ```

3. Type `1` and press Enter.
4. Answer the questions of the installer. The table below explains each one.
5. The installer shows the disk and asks you to type its serial number. This is the last chance to stop. To cancel, press Ctrl+C.
6. Wait for the install. It takes a few minutes.
7. Write down the recovery key that the installer prints at the end. Keep it away from the computer.
8. Confirm that you wrote down the key. The computer reboots.
9. Remove the USB stick.

| Question | What to enter |
|---|---|
| Disk | The internal disk for Sibyl. The installer hides the USB stick and disks that are too small or in use. |
| Administrator user name | The account that you log in with. It can use `sudo`. The default is `admin`. |
| Password | At least 8 characters. You use it at the console and for `sudo`. |
| SSH public keys | Optional. One key per line, such as the content of `~/.ssh/id_ed25519.pub`. SSH accepts keys only, not passwords. |
| GitHub user | Optional. The installer adds the public keys from `https://github.com/<user>.keys`. |
| Disk passphrase | Optional when the computer has a TPM. Required without one. At least 12 characters. |

The recovery key is eight groups of eight letters:

```
    cbdefghi-jklnrtuv-cbdefghi-jklnrtuv-cbdefghi-jklnrtuv-cbdefghi-jklnrtuv
```

You need the recovery key if the TPM refuses to unlock the disk, for example after a firmware update. Without the key and without a passphrase, the data on the disk is lost. See [Disk encryption and recovery](../guides/recovery.md).

Next, go to [First boot and setup](first-boot.md).

### Install over the network

You can also boot the installer over the network instead of from a USB stick. See [tools/pxe/README.md](../../tools/pxe/README.md).

## Try Sibyl from the USB without an install

The live USB runs Sibyl without changes to the internal disks. The services need a persistence partition on the stick, because their images do not fit in memory. A persistence partition is an extra partition on the stick that keeps the images, models and settings.

1. Use a USB stick of 32 GB or more.
2. Boot from the stick, and choose `2` in the menu. Sibyl adds a partition in the free space of the stick.
3. Reboot from the stick. The services start from the persistence partition.
4. Choose `3` in the menu to run the setup wizard.

The live system logs in as root at the console without a password. The persistence partition is not encrypted. Use the live system only to try Sibyl, on a network that you trust.
