# Sibyl

Sibyl is an all-in-one inference server. It turns a computer with one or more GPUs into a private AI server. You install it from a USB stick or over the network, and you run it from a web browser or a terminal console.

Sibyl is pre-alpha.

## What you get

- Local models. llama.cpp serves GGUF models through llama-swap, which loads and swaps models on demand. One model can span several GPUs, or each GPU can run its own model.
- Standard APIs. Sibyl serves an OpenAI-compatible API (`/v1`), an Anthropic Messages API and an Ollama-compatible API (`/api`), behind TLS and a key for each program. Existing clients work unchanged.
- A model router. The model `auto` picks a local model or a cloud provider for each request, by rules that you set, with a budget for each program. A rule can keep every request that holds a secret on the host.
- Chat. Open WebUI gives a chat interface with document search through a local embedding model.
- Model management. Sibyl searches Hugging Face, shows which files fit your GPUs, and downloads with resume and checksums. Each model has its own context size, cache, sampling, reasoning and speculative decoding settings.
- Two consoles. The web console at `https://<address>:8443/console/` and the terminal console `sibyl` both cover setup, status, models, downloads, the router, the network, accounts, storage and updates.
- A secure host. The host is Fedora bootc: the operating system is a signed image that updates in one step and rolls back. SELinux is enforcing, Secure Boot is on, and the disk is encrypted with LUKS2 and unlocked by the TPM2.
- Other containers. Arcane runs your own containers next to the inference stack.

## Hardware

Each GPU vendor has its own host image, because the drivers differ.

| Variant | GPUs | Backends | Status |
|---|---|---|---|
| `amd` | Radeon and Ryzen AI. HIP runs on RX 6800 and 6900, RX 7000, RX 9000, Radeon PRO W6800, W7000 and AI PRO R9700, and Ryzen AI 300 and Strix Halo APUs. Every other Radeon uses Vulkan. | HIP (ROCm) and Vulkan | Tested on two Radeon RX 7900 XTX cards |
| `nvidia` | GeForce and data-center GPUs from Turing (RTX 20 series) on | CUDA and Vulkan | Builds, not tested on hardware |
| `intel` | Arc and recent integrated graphics | Vulkan | Builds, not tested on hardware |

The HIP image is built for the ROCm targets `gfx1030`, `gfx1100`, `gfx1101`, `gfx1102`, `gfx1150`, `gfx1151`, `gfx1200` and `gfx1201`. Sibyl reads each card's target from the kernel, and it uses Vulkan for a card outside that list. Only `gfx1100` is tested on hardware.

If you can lend an NVIDIA or Intel GPU, or give access to a machine with one, open an issue. That variant can then be tested and fixed.

## Quick start

1. Download the live ISO for your variant: `oras pull ghcr.io/diffsec/sibyl-iso:latest-amd`.
2. Write it to a USB stick, and boot the computer from it with Secure Boot on.
3. In the menu, choose "Install Sibyl to a disk". The installer erases the disk that you choose and prints a recovery key. Keep that key.
4. After the reboot, log in as the administrator that you created, and run `sibyl setup`. You can also open `https://<address>:8443/console/setup` in a browser.

[Install](docs/getting-started/install.md) has every step, and the network install is in [tools/pxe/README.md](tools/pxe/README.md).

## Documentation

The [documentation index](docs/index.md) lists every page. Start here:

- [Install](docs/getting-started/install.md) and [First boot](docs/getting-started/first-boot.md) take you from a USB stick to a working server.
- [Guides](docs/index.md#guides) cover daily use: the consoles, API clients, models, the router, updates and recovery.
- [Reference](docs/index.md#reference) lists every System API route and every setting.
- [Development](docs/development.md) and [Design](docs/index.md#design) are for contributors.

## Contributing

Contributions need a signed Contributor License Agreement. [CONTRIBUTING.md](CONTRIBUTING.md) explains how to sign it and which checks to run.

## License

Sibyl is licensed under the GNU Affero General Public License v3.0. See [LICENSE](LICENSE). Commercial licenses are available from the maintainer.
