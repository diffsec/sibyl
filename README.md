# Sibyl

Sibyl is an all-in-one inference server. It turns a computer with one or more GPUs into a private AI server that you install from a USB stick or over the network, and then run from a web browser and a terminal console.

## What you get

- llama.cpp serves GGUF models through llama-swap, which loads and swaps models on demand. One model can span several GPUs, or several models can each have a GPU.
- An OpenAI-compatible API (`/v1`) and an Ollama-compatible API (`/api`) sit behind TLS and an API key, so existing clients work unchanged.
- Open WebUI gives chat, with document search through a local embedding model.
- The model manager searches Hugging Face, shows which files fit your GPUs, and downloads with resume and checksums. Each model has its own settings: context size, KV cache, sampling and speculative decoding. A built-in benchmark compares the GPU backends on your hardware.
- Arcane runs other containers next to the inference stack.
- `sibyl` is a terminal interface for setup, status, models, downloads, network, accounts and storage. `sibyl setup` walks through the first setup.
- The host is Fedora bootc. The operating system is an image that updates atomically and rolls back. SELinux is enforcing, Secure Boot is on, every Sibyl image is signed, and the host refuses Sibyl images that are not.
- The installer encrypts the disk with LUKS2 and unlocks it with the TPM2, with a recovery key. Extra disks become encrypted model pools.
- Updates come online with `bootc upgrade`, or offline from a signed bundle on a USB drive.

## Hardware

Each GPU vendor has its own host image, because the drivers differ.

| Variant | GPUs | Inference backends | Status |
|---|---|---|---|
| `amd` | Radeon. HIP needs an RX 7900 series card (`gfx1100`); every other Radeon uses Vulkan | HIP (ROCm) and Vulkan | Tested on two Radeon RX 7900 XTX cards |
| `nvidia` | GeForce and data-center GPUs from Turing (RTX 20 series) on | CUDA and Vulkan | Builds; not tested on hardware |
| `intel` | Arc and recent integrated graphics | Vulkan | Builds; not tested on hardware |

The HIP image is built for `gfx1100` only, because each extra ROCm target adds build time. Other targets can be added to `GPU_TARGETS` in `containers/llama-swap/Containerfile.hip`.

The NVIDIA and Intel variants build in CI and pass the same checks as the AMD one, but no one has run them on real hardware yet. If you can lend or donate an NVIDIA or Intel GPU, or give access to a machine with one, open an issue. We will test that variant and fix what breaks.

The NVIDIA driver is built into the image and signed with the project's Machine Owner Key (MOK). Secure Boot loads it only after you enroll that key once: run `sudo mokutil --import /usr/share/sibyl/sibyl-mok.der`, reboot, and confirm the key in the blue MOK screen.

## Install

1. Download the live ISO for your variant: `oras pull ghcr.io/ihavespoons/sibyl-iso:latest-amd`.
2. Write it to a USB stick, and boot the computer from it with Secure Boot on.
3. In the menu, choose "Install Sibyl to a disk". The installer erases the disk that you choose and prints a recovery key. Keep that key.
4. After the reboot, log in as the administrator that you created and run `sibyl setup`.

To install over the network instead, see [tools/pxe/README.md](tools/pxe/README.md).

## Status

Sibyl is pre-alpha. The design and the phase plan are in [docs/design.md](docs/design.md).

## Build

The images build for `linux/amd64`. CI builds, pushes and signs them on each push to `main` (see `.github/workflows/bootc.yml`). The NVIDIA and Intel host images build only for release tags, or with `gh workflow run bootc.yml -f variants=true`.

Build a host image locally:

```bash
podman build -f os/Containerfile --ignorefile os/Containerfile.dockerignore --build-arg GPU_VARIANT=amd -t sibyl-os:amd .
```

Build the inference images locally:

```bash
podman build -f containers/llama-swap/Containerfile.hip -t sibyl-llama-swap:hip containers/llama-swap
podman build -f containers/llama-swap/Containerfile.vulkan -t sibyl-llama-swap:vulkan containers/llama-swap
podman build -f containers/llama-swap-cuda/Containerfile -t sibyl-llama-swap:cuda containers
```

The HIP build needs about 40 GB of free disk space, because the ROCm build image is large. The CUDA image builds on llama.cpp's own CUDA image, so it compiles nothing.

## License

Sibyl is licensed under the GNU Affero General Public License v3.0. See [LICENSE](LICENSE). Commercial licenses are available from the copyright holder. Contributions require a contributor license agreement, as described in [CONTRIBUTING.md](CONTRIBUTING.md).
