# Sibyl user guide

This guide takes you from a computer with a GPU to a private AI server that answers chat and API requests. It covers the install, the first setup, daily use, updates and recovery. You do not need to know Linux well, but you must be able to change firmware settings and use a terminal.

Sibyl is pre-alpha. The AMD variant is tested on one host with two Radeon RX 7900 XTX cards. The NVIDIA and Intel variants build and pass the same checks, but no one has run them on real hardware yet.

## Contents

1. [What you need](#what-you-need)
2. [Get the installer](#get-the-installer)
3. [Prepare the computer](#prepare-the-computer)
4. [Install Sibyl](#install-sibyl)
5. [First boot and setup](#first-boot-and-setup)
6. [Use Sibyl](#use-sibyl)
7. [Models](#models)
8. [Host settings and accounts](#host-settings-and-accounts)
9. [Run other containers with Arcane](#run-other-containers-with-arcane)
10. [Updates](#updates)
11. [Disk encryption and recovery](#disk-encryption-and-recovery)
12. [Try Sibyl from the USB without an install](#try-sibyl-from-the-usb-without-an-install)
13. [Troubleshooting](#troubleshooting)

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

A backend is the GPU code that llama.cpp uses. Sibyl can measure which backend is faster on your hardware.

## Get the installer

The installer is an ISO file. It is published as an OCI artifact on GitHub's container registry, because it is too large for a GitHub release.

1. Install `oras` on the second computer. See [oras.land](https://oras.land/docs/installation).
2. Download the ISO for your variant. This example is for `amd`:

```bash
mkdir sibyl-iso && cd sibyl-iso
oras pull ghcr.io/ihavespoons/sibyl-iso:latest-amd
```

3. Make sure that the file is complete. The command must print `OK`. On Linux, use `sha256sum -c SHA256SUMS` instead:

```bash
shasum -a 256 -c SHA256SUMS
```

The ISO file name contains the build tag, such as `sibyl-sha-339b7a4-amd.iso`. The examples below write it as `sibyl-*.iso`.

4. Write the ISO to the USB stick. This erases the stick.

On Linux, find the stick with `lsblk`, then write the ISO. Replace `/dev/sdX` with the stick, not with a disk that you need:

```bash
sudo dd if=sibyl-*.iso of=/dev/sdX bs=4M conv=fsync status=progress
```

On macOS, find the stick with `diskutil list`, then:

```bash
diskutil unmountDisk /dev/diskN
sudo dd if=sibyl-*.iso of=/dev/rdiskN bs=4m
```

On Windows, use [balenaEtcher](https://etcher.balena.io/), or Rufus in DD image mode.

## Prepare the computer

Make these changes in the firmware setup. The key to open it is usually Del, F2 or F10 at power-on.

1. Turn on Secure Boot. If the firmware is in Setup Mode, install the default keys first.
2. Turn on the TPM. It is often called fTPM, PTT or "Security Device Support".
3. Set the boot order to USB first, or note the key for the one-time boot menu, often F8, F11 or F12.
4. Set a firmware password, and lock the boot order. The TPM unlocks the disk for any system that boots through the same signed boot loader, so a locked firmware is part of the protection.

NVIDIA only: the NVIDIA driver is signed with the Sibyl project key. Secure Boot loads it only after you enroll that key. You do this after the install, in [NVIDIA key enrollment](#nvidia-key-enrollment).

## Install Sibyl

1. Plug in the USB stick and the network cable. Boot from the stick.
2. The live system starts and shows a menu on the screen:

```
  1  Install Sibyl to a disk
  2  Make a persistence partition on this USB
  3  Setup wizard (needs persistence)
  4  Shell
  5  Reboot
  6  Power off
```

3. Type `1` and press Enter. The installer asks these questions:

| Question | What to enter |
|---|---|
| Disk | The internal disk for Sibyl. The installer hides the USB stick and disks that are too small or in use. |
| Administrator user name | The account that you log in with. It can use `sudo`. The default is `admin`. |
| Password | At least 8 characters. You use it at the console and for `sudo`. |
| SSH public keys | Optional. One key per line, such as the content of `~/.ssh/id_ed25519.pub`. SSH accepts keys only, not passwords. |
| GitHub user | Optional. The installer adds the public keys from `https://github.com/<user>.keys`. |
| Disk passphrase | Optional when the computer has a TPM. Required without one. At least 12 characters. |

4. The installer shows the disk and asks you to type its serial number. This is the last chance to stop. Press Ctrl+C to cancel.
5. The install takes a few minutes. At the end, the installer prints the recovery key, eight groups of eight letters:

```
    cbdefghi-jklnrtuv-cbdefghi-jklnrtuv-cbdefghi-jklnrtuv-cbdefghi-jklnrtuv
```

Write the recovery key down and keep it away from the computer. You need it if the TPM refuses to unlock the disk, for example after a firmware update. Without it and without a passphrase, the data on the disk is lost.

6. Confirm that you wrote down the key. The computer reboots. Remove the USB stick.

## First boot and setup

The first boot creates the TLS certificate and the API key, then downloads the service images. This takes 5 to 15 minutes, depending on your internet connection. The disk unlocks without a passphrase when the TPM is in use.

### Log in

Log in at the console with the administrator account, or over SSH from the second computer if you added a key:

```bash
ssh admin@<address>
```

To find the address, run `ip -br addr` at the console, or look in your router's list of DHCP clients.

### NVIDIA key enrollment

Skip this section on AMD and Intel.

1. Run `sudo mokutil --import /usr/share/sibyl/sibyl-mok.der`. It asks you to choose a one-time password.
2. Reboot with `sudo systemctl reboot`.
3. A blue screen, the MOK manager, appears. Choose "Enroll MOK", then "Continue", then "Yes", and type the one-time password.
4. The computer boots again, and the NVIDIA driver loads. `nvidia-smi` lists the GPUs.

### Run the setup wizard

Run:

```bash
sibyl setup
```

The wizard walks through the setup screens in order. Each step tells you which key to press. Press `]` for the next step and `[` to go back. You can run `sibyl setup` again at any time, and you can change every setting later from the normal screens.

1. GPUs, backend and layout. Press `e`. Choose the GPUs for inference, the backend, and the default layout. "Span" splits one model across all the selected GPUs. "Pin" runs one model on each GPU, so several models can stay loaded at once. If you are not sure, keep the default backend and measure later with the benchmark.
2. Hostname and network. Press `h` for the hostname and `n` for the network. A network change rolls back by itself after 120 seconds unless you confirm it. Reconnect at the new address and press `c` to keep it.
3. Operator accounts. Press `a` to add a person who can run `sibyl` over SSH. Operators cannot use `sudo`.
4. Open WebUI administrator. Press `o` and create the first Open WebUI account. Do this now: until an account exists, anyone on your network who opens Open WebUI first can create the administrator account.
5. First model. Press `/` and search Hugging Face. See [Download a model](#download-a-model).
6. Done. The Status screen lists the addresses of the services as links. The last step also shows the SHA-256 fingerprint of the TLS certificate.

### Trust the certificate

Sibyl makes its own TLS certificate at the first boot, so browsers warn once. Before you accept the warning, compare the fingerprint that the browser shows with the fingerprint from the last wizard step. They must be the same.

You can also copy the certificate and trust it on the second computer:

```bash
scp admin@<address>:/var/lib/sibyl-operator/ca.crt sibyl-ca.crt
```

The certificate is valid for `sibyl.local`, `sibyl` and the IP address that the host had at the first boot.

### Arcane password

Open `https://<address>:9443/` and log in to Arcane with the user `arcane` and the password `arcane-admin`. Arcane makes you choose a new password at once. Do this right after the install, because Arcane controls every container on the host.

## Use Sibyl

| Service | Address | Use |
|---|---|---|
| Open WebUI | `https://<address>/` | Chat in the browser, with document search |
| OpenAI-compatible API | `https://<address>:8443/v1` | Programs and tools that speak the OpenAI API |
| Ollama-compatible API | `https://<address>:8443/api` | Programs and tools that speak the Ollama API |
| Arcane | `https://<address>:9443/` | Other containers on the same host |

### The API key

Both APIs need the API key. It is in `/var/lib/sibyl-operator/api.key`, which the administrator and operators can read. Treat it like a password: the same key also controls the Sibyl System API, which manages models, accounts and disks.

```bash
cat /var/lib/sibyl-operator/api.key
```

### Call the OpenAI API

```bash
KEY="$(ssh admin@<address> cat /var/lib/sibyl-operator/api.key)"
curl --cacert sibyl-ca.crt -H "Authorization: Bearer $KEY" https://<address>:8443/v1/models
```

```bash
curl --cacert sibyl-ca.crt -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  https://<address>:8443/v1/chat/completions \
  -d '{"model":"<model id>","messages":[{"role":"user","content":"Hello"}]}'
```

In a program, set the base URL to `https://<address>:8443/v1` and the API key to the key. The model ID is a name from `/v1/models`.

The first request to a model loads it into GPU memory. That takes from a few seconds to a few minutes for a large model.

### Call the Ollama API

Set the Ollama host to `https://<address>:8443` and send the key as a bearer token. A client that cannot send an `Authorization` header cannot use Sibyl.

```bash
curl --cacert sibyl-ca.crt -H "Authorization: Bearer $KEY" https://<address>:8443/api/tags
```

Sibyl downloads models itself, so `ollama pull` and the other model management calls return an error that says so.

### The sibyl console

Run `sibyl` on the host, at the console or over SSH. It is a terminal interface with five screens. Press a number or Tab to change screens, and `q` to quit.

| Key | Screen | What you can do |
|---|---|---|
| `1` | Status | See the addresses, GPU memory and the service states. `r` restarts a service. `o` connects Open WebUI. |
| `2` | Hardware | `e` changes the GPUs, the backend and the default layout. |
| `3` | Models | `enter` edits a model's settings. `b` runs the benchmark. `d` deletes the model. |
| `4` | Download | `/` searches Hugging Face. `t` sets a Hugging Face token. `c` cancels a download, `R` retries one. |
| `5` | Host | `h` hostname, `n` network, `a` add operator, `k` keys, `d` delete operator, `p` new model pool, `x` detach pool. |

The bottom line of each screen lists its keys.

## Models

Sibyl runs GGUF models with llama.cpp. llama-swap loads a model when a request asks for it, and unloads models when the GPUs need room for another.

### Download a model

1. Open the Download screen (`4`) and press `/`.
2. Type a search, such as `qwen3 gguf`, and press Enter. You can also paste a full name, such as `Qwen/Qwen3-Embedding-0.6B-GGUF`, to open that repository.
3. Choose a repository, then a variant. A variant is one quantization of the model, such as `Q4_K_M` or `Q8_0`. Smaller quantizations need less memory and lose some quality.
4. The fit column tells you how the variant suits your GPUs:

| Fit | Meaning |
|---|---|
| `gpu` | The file fits in the VRAM of the selected GPUs, with room for the context. |
| `split` | The model needs system RAM as well, and it runs slower. |
| `no` | The model is too large for this host. |
| `?` | The GPU does not report its VRAM. This is normal on NVIDIA and Intel. |

5. If the host has model pools, choose where the model goes. Then confirm. The download runs in the background, and you can leave the screen. Sibyl checks each file against the checksum from Hugging Face, and the model appears on the Models screen when it is complete.

A download that stops resumes where it stopped. Press `R` to retry a failed download.

### Gated and private models

Some models need a Hugging Face account. Accept the model's terms on huggingface.co first. Then create an access token with read access at [huggingface.co/settings/tokens](https://huggingface.co/settings/tokens), press `t` on the Download screen, and paste it. Sibyl checks the token with Hugging Face and shows the account name.

### Model settings

On the Models screen, select a model and press Enter. The settings apply as soon as you save them.

| Setting | Effect |
|---|---|
| Layout | Span across all selected GPUs, or pin to one GPU. |
| Context size | The longest conversation in tokens. Empty uses the model's native context, which is often 262,144 tokens and needs a lot of VRAM. The field shows the native context and an estimate of the memory that your choice needs, and warns when it may not fit. |
| Embedding | Marks a model for embeddings. Open WebUI uses the first embedding model for document search. |
| Hidden | Leaves the model out of the model lists. |
| Sampling | Default temperature, top-k, top-p, min-p and penalties. An empty field shows in grey the value that applies: the model file's recommendation, or llama.cpp's default. A client can still override them per request. |
| KV cache | The precision of the context memory. `q8_0` halves its size with little loss. |
| Speculative decoding | Speeds up generation with a small draft model or with n-gram lookup. |

### Benchmark the backends

Select a model on the Models screen and press `b`. Sibyl unloads the running models and measures each backend of your variant on that model. It takes a few minutes, and the result shows on the Models screen with a recommendation. To use the faster backend, change it on the Hardware screen.

### Model pools

A model pool is an extra disk for models. Sibyl encrypts it and mounts it under `/var/lib/sibyl-pools`.

1. On the Host screen, press `p` and choose the disk. The disk is erased.
2. Type the disk's serial number to confirm.
3. Sibyl shows the pool's recovery key once. Write it down. You need it to move the pool to another computer.

Downloads can then go to the pool. To remove a pool, press `x`. The data stays on the disk.

## Host settings and accounts

### Operators

An operator can log in over SSH and run `sibyl`, but cannot use `sudo`. Add operators on the Host screen with `a`, with SSH keys or a GitHub user name. An operator can read the API key, so add only people whom you trust with the whole server.

To let an existing account run `sibyl`, an administrator runs:

```bash
sudo usermod -aG sibyl-operators <user>
```

The user must log out and back in.

### Network

On the Host screen, press `n` to choose DHCP or a static IPv4 address. The change rolls back after 120 seconds unless you press `c`. If you connect over SSH, reconnect at the new address within that time and press `c` there.

The TLS certificate keeps the address that it was made with. After an address change, use the name `sibyl.local` or trust the warning again.

## Run other containers with Arcane

Arcane at `https://<address>:9443/` runs other containers and Compose stacks beside Sibyl. It uses the same Podman as the Sibyl services, so it can also stop them. A stopped Sibyl service starts again at the next boot.

The host accepts images from the common public registries: Docker Hub, Quay, GitHub, Google, Microsoft, Amazon ECR Public, NVIDIA, GitLab, Chainguard and LinuxServer. An administrator can allow another registry in `/etc/containers/policy.json`.

## Updates

Sibyl does not update by itself. The operating system and the services update together, as one image.

### Online update

```bash
sudo bootc upgrade
sudo systemctl reboot
```

`bootc upgrade` downloads the new image and checks its signature. The new version starts at the next boot. `sudo bootc status` shows the running version and the one that is staged.

### Roll back

If a new version has a problem, go back to the previous one:

```bash
sudo bootc rollback
sudo systemctl reboot
```

Your models, settings and accounts are in `/var`, which an update and a rollback do not change.

### Offline update

A host with no internet updates from a bundle on a USB drive.

1. On a computer with internet, download the bundle for your variant to a USB drive of 8 GB or more:

```bash
oras pull ghcr.io/ihavespoons/sibyl-bundle:latest-amd -o /path/to/usb/sibyl-bundle
```

2. Plug the drive into the Sibyl host and mount it:

```bash
lsblk
sudo mount /dev/sdX1 /mnt
```

3. Apply the bundle and reboot:

```bash
sudo sibyl-update apply /mnt/sibyl-bundle
sudo systemctl reboot
```

`sibyl-update` checks the bundle's signature and every file before it changes anything. After an offline update, the host follows the bundle. When it has internet again, return to online updates with the command that `sibyl-update` prints.

## Disk encryption and recovery

The root disk is encrypted with LUKS2. The TPM unlocks it at boot while the Secure Boot settings stay the same.

### When the TPM does not unlock the disk

A firmware update, a change to Secure Boot, or new Secure Boot keys can stop the TPM from unlocking the disk. The boot then asks for a passphrase. Type the recovery key, with its dashes.

After the boot, bind the disk to the TPM again so the next boot unlocks by itself. Find the encrypted partition with `lsblk`, then:

```bash
sudo systemd-cryptenroll --wipe-slot=tpm2 --tpm2-device=auto --tpm2-pcrs=7 /dev/nvme0n1p3
```

The command asks for the recovery key. Replace `/dev/nvme0n1p3` with your partition.

### Check the encryption

```bash
sudo systemd-cryptenroll /dev/nvme0n1p3
```

The list must show a `tpm2` slot and a `recovery` slot.

## Try Sibyl from the USB without an install

The live USB runs Sibyl without changing the internal disks. The services need a persistence partition on the stick, because their images do not fit in memory.

1. Use a USB stick of 32 GB or more.
2. Boot from it, and choose `2` in the menu. Sibyl adds a partition in the free space of the stick.
3. Reboot from the stick. The services start from the persistence partition.
4. Choose `3` in the menu to run the setup wizard.

The live system logs in as root at the console without a password, and the persistence partition is not encrypted. Use it only to try Sibyl, on a network that you trust.

## Troubleshooting

### A service does not start

The Status screen in `sibyl` shows each service. From a shell, as the administrator:

```bash
sudo systemctl --user -M sibyl@ status llama-swap
sudo journalctl _SYSTEMD_USER_UNIT=llama-swap.service -n 50
```

The services are `llama-swap`, `open-webui`, `system-api`, `caddy` and `arcane`. To restart one, select it on the Status screen and press `r`.

### A model does not load

1. Look at the llama-swap log with the command above. An "out of memory" error means that the model, or its context size, is too large for the selected GPUs. Choose a smaller quantization or a smaller context size.
2. On AMD, if the HIP backend fails on a card that Sibyl does not recognize, change the backend to Vulkan on the Hardware screen. Sibyl refuses HIP for the cards that it knows the HIP image does not support.

### `sibyl` says that you are not in the sibyl-operators group

An administrator adds you with `sudo usermod -aG sibyl-operators <user>`. Log out and back in.

### The browser cannot reach Sibyl

1. Make sure that you use `https://`, and the right port: no port for Open WebUI, 8443 for the APIs, 9443 for Arcane.
2. The host firewall allows only these ports and SSH. Make sure that no firewall between your computer and the host blocks them.

### Report a problem

Open an issue at [github.com/ihavespoons/sibyl/issues](https://github.com/ihavespoons/sibyl/issues). Include the output of `sudo bootc status`, the Status screen, and the log of the service that fails. Do not include the API key.
