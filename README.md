# Sibyl

Sibyl is a hardened, immutable Linux host for local LLM inference. It boots from USB or installs to disk. It serves an OpenAI-compatible API from llama.cpp, with Open WebUI for chat and Arcane for container management.

Sibyl is tuned first for AMD RDNA3. The inference images are built for `gfx1100` (Radeon RX 7900 XTX, XT and GRE), and one model can span two cards.

## Status

Sibyl is pre-alpha. Phase 1 (host image, inference images, Quadlet units, CI) is written. Phase 2 has started with the System API. The design and the phase plan are in [docs/design.md](docs/design.md).

## Stack

| Layer | Component |
|---|---|
| Host | Fedora bootc 44, SELinux enforcing, nftables default deny |
| Containers | Rootless Podman, managed by systemd Quadlet units |
| Inference | llama.cpp (HIP or Vulkan) behind llama-swap |
| Chat UI | Open WebUI |
| Container UI | Arcane |
| TLS | Caddy on Wolfi |
| Management | Sibyl System API (Go), with a host agent for unit control |

## Build

The images build for `linux/amd64`. CI builds, pushes and signs them on each push to `main` (see `.github/workflows/bootc.yml`).

Build the host image locally:

```bash
podman build -f os/Containerfile --ignorefile os/Containerfile.dockerignore --build-arg GPU_VARIANT=amd -t sibyl-os:amd .
```

Build the inference images locally:

```bash
podman build -f containers/llama-swap/Containerfile.hip -t sibyl-llama-swap:hip containers/llama-swap
podman build -f containers/llama-swap/Containerfile.vulkan -t sibyl-llama-swap:vulkan containers/llama-swap
```

The HIP build needs about 40 GB of free disk space, because the ROCm build image is large.

## Try the inference image on an existing host

Run this on a Linux host with the `amdgpu` driver loaded. Put GGUF files in `~/models` and edit `containers/llama-swap/config.yaml` to point at them.

```bash
podman run --rm -p 8080:8080 \
  --device /dev/kfd --device /dev/dri --group-add keep-groups \
  -v ~/models:/models:ro,z \
  -v ./containers/llama-swap:/app/config:ro,z \
  ghcr.io/ihavespoons/sibyl-llama-swap:latest-hip
```

## License

Sibyl is licensed under the GNU Affero General Public License v3.0. See [LICENSE](LICENSE). Commercial licenses are available from the copyright holder. Contributions require a contributor license agreement, as described in [CONTRIBUTING.md](CONTRIBUTING.md).
