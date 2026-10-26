# Sibyl design

Sibyl is a hardened, immutable Linux host for local LLM inference. It boots from USB or installs to disk, and serves an OpenAI-compatible API from llama.cpp. It is tuned first for AMD RDNA3 GPUs, with one or more Radeon RX 7900 XTX cards.

This document records the design decisions from October 2026 and the delivery phases.

## Decisions

| Area | Decision | Reason |
|---|---|---|
| Host OS | Fedora bootc (`quay.io/fedora/fedora-bootc:44`) | The OS is a Containerfile. Updates are atomic, with rollback. The kernel is current for gfx1100. |
| Container runtime | Podman with Quadlet, rootless | Fedora default. Quadlet units start at boot without a daemon. |
| Container UI | Arcane (BSD-3-Clause) | Compose editor, Git Sync, native Podman and Quadlet pages. Dockhand is source-available (BSL 1.1), so it is out. |
| Inference engine | llama.cpp `llama-server` behind llama-swap | Best single-stream speed for GGUF on RDNA3. llama-swap swaps models and places them on GPUs. |
| GPU backends | HIP (ROCm) and Vulkan images, both shipped | Results on RDNA3 differ by quant and context size. A benchmark picks one per model. |
| Model format | GGUF only | Fits 24 GB per card. No vLLM. |
| Multi-GPU | Per-model layout through the llama-swap matrix router | One model can span both cards, or two models can run on one card each. |
| Other GPUs | Separate image variants: `-amd`, `-nvidia`, `-intel` | Only the NVIDIA variant carries signed out-of-tree kernel modules. |
| Base for service images | Wolfi where possible | Smaller images with fewer CVEs. ROCm images stay on the AMD base, because Wolfi does not package ROCm. |
| Boot | Live USB with persistence, and install to disk | Try it from USB, then install. |
| Disks | Guided installer, LUKS with TPM2 unlock, model storage pools, USB persistence | See "Storage". |
| Hardening | SELinux enforcing, Secure Boot, signed images, CIS-style lockdown | See "Security". |
| Updates | GHCR registry, plus offline bundles | `bootc upgrade` online. `bootc switch` to an OCI archive offline. |
| CI | GitHub Actions, SHA-pinned actions, cosign keyless signing | Images and ISOs build in CI. |
| Model download | Hugging Face GGUF picker in the System API and TUI, plus an Ollama-compatible API shim | Clients written for Ollama work unchanged. |

## Repository layout

```
os/
  Containerfile              # bootc host image, ARG GPU_VARIANT=amd|nvidia|intel
  packages/                  # dnf package lists per variant
  rootfs/                    # files copied into / of the host image
containers/
  llama-swap/                # Containerfile.hip, Containerfile.vulkan
  caddy/                     # Wolfi-based TLS front door
  system-api/                # Wolfi-based management API (phase 2)
  tui/                       # Wolfi-based console TUI and setup wizard (phase 2)
scripts/                     # local build helpers, ISO creation, benchmark
docs/                        # this document
.github/workflows/bootc.yml  # build, sign, push
```

## Runtime layout

All Sibyl containers run rootless under one system user, `sibyl` (UID 1001). Rootless Podman confines each container to that user's subordinate UID range. A container escape gives an unprivileged user, not root.

Arcane uses the same user's Podman socket. It therefore sees and controls the core containers. It runs with `SecurityLabelDisable=true`, because the Arcane docs require it on SELinux hosts. The socket proxy from the earlier design is not used. A proxy cannot filter the Quadlet and D-Bus calls that Arcane needs, and rootless Podman already removes root from the blast radius.

systemd owns the core services. The Quadlet files are in the OS image under `/etc/containers/systemd/users/1001/`. Arcane can stop a core container, but the next boot or `systemctl --user restart` brings it back from the image definition. User stacks that Arcane creates live under `/var/lib/sibyl/stacks`.

| Container | Image | Port on the `sibyl` network | Exposed on the host |
|---|---|---|---|
| `llama-swap` | `ghcr.io/ihavespoons/sibyl-llama-swap:<ver>-hip` or `-vulkan` | 8080 | No |
| `open-webui` | `ghcr.io/open-webui/open-webui` (pinned digest) | 8080 | No |
| `system-api` | `ghcr.io/ihavespoons/sibyl-system-api` | 3001 | No |
| `arcane` | `ghcr.io/getarcaneapp/manager` (pinned digest) | 3552 | No |
| `caddy` | `ghcr.io/ihavespoons/sibyl-caddy` | 10443, 18443, 19443 | Yes, through nftables redirect |

nftables redirects host port 443 to 10443 (Open WebUI), 8443 to 18443 (OpenAI API and System API), and 9443 to 19443 (Arcane). The input chain accepts the high ports only for redirected connections. This keeps `net.ipv4.ip_unprivileged_port_start` at its default of 1024.

## System API and host agent

The System API is a Go service (`cmd/sibyl-api`, `internal/`). It runs in a container with no access to the systemd bus, so SELinux confinement stays on. It reads and writes only these directories:

| Host path | In the container | Content |
|---|---|---|
| `/var/lib/sibyl/config` | `/config` | `hardware.yaml`, `models.yaml`, `llama-swap/config.yaml` |
| `/var/lib/sibyl/models` | `/models` | GGUF files |
| `/var/lib/sibyl/run` | `/run/sibyl` | Request queue and `status.json` |
| `/var/lib/sibyl/home/.config/containers/systemd/llama-swap.container.d` | `/quadlet` | `backend.conf`, which overrides the image default |

Each change to the hardware or to model settings runs an apply. The apply regenerates the llama-swap configuration, and llama-swap reloads it without a restart. llama-swap polls the file with `stat`, so the configuration is mounted as a directory: a single-file bind mount keeps the old inode after an atomic rename. If the backend or the GPU set changes, the apply also rewrites `backend.conf` and queues a reload and a restart.

The host agent (`/usr/libexec/sibyl/agent`) does the unit control. The System API creates an empty file named `<timestamp>.<action>.<unit>` in the request queue. The `sibyl-agent.path` user unit starts the agent, which accepts only `restart` and `reload` for the five Sibyl units and deletes every request. The `sibyl-status.timer` writes the unit states to `status.json` every 15 seconds.

| Route | Purpose |
|---|---|
| `GET /health` | Liveness, no token |
| `GET /status` | Version, GPU count, hardware choice, unit states |
| `GET /gpus` | Detected GPUs from sysfs, in PCI order |
| `GET, PUT /hardware` | Backend, selected GPUs, default layout. PUT applies. |
| `GET /models` | Installed models with their settings |
| `PUT /models/{id}` | Layout, context size, embedding flag, extra arguments. Applies. |
| `DELETE /models/{id}` | Deletes the model and all of its shards. Applies. |
| `POST /apply` | Regenerates the configuration |
| `GET /services`, `POST /services/{name}/restart` | Unit states and restarts through the host agent |
| `GET /hf/search?q=` | Search Hugging Face for GGUF repositories, most downloaded first |
| `GET /hf/repos/{owner}/{name}` | Complete GGUF variants at a pinned commit, with size and fit (`gpu`, `split` or `no`) |
| `GET, POST /downloads` | List download jobs, or queue one: `{"repo", "variant", "revision"}` |
| `GET, DELETE /downloads/{id}`, `POST /downloads/{id}/retry` | One job: progress, cancel, or resume a failed job |

Extra llama-server arguments must match `^[A-Za-z0-9_.,:=/+-]+$`. Flags that Sibyl sets, such as `--model`, `--port` and `--device`, are rejected.

### Console interface

The TUI (`cmd/sibyl-tui`, `internal/tui`) is a container image, `sibyl-tui`. It talks only to the System API, so it has no host privileges. Members of the `sibyl-operators` group run it with `sibyl`, with no sudo. The command starts the TUI in the caller's own rootless Podman, read-only with all capabilities dropped. The TUI connects through Caddy on `127.0.0.1:18443` and verifies the host certificate as `sibyl.local`.

At each boot, `sibyl-operator-export.service` copies the API key and the certificate to `/var/lib/sibyl-operator/`, readable by `sibyl-operators` only. Membership gives full control of the System API, so add only trusted users: `sudo usermod -aG sibyl-operators <user>`.

| Screen | Purpose |
|---|---|
| Status | GPUs with VRAM in use, and service states with restarts |
| Hardware | The GPU part of the setup wizard: GPUs for inference, backend, default layout |
| Models | Installed models, the settings editor, and delete |
| Download | Hugging Face search, variants with fit, confirmation, and download jobs |

The network, account and SSH key steps of the setup wizard need host-level changes that the System API does not offer yet. Phase 3 adds them.

### Model downloads

The download manager (`internal/download`) runs one job at a time. Each job pins a commit, so every file comes from one revision even if the repository changes during the download.

1. The job checks that the disk has room for the rest of the download plus 2 GiB.
2. Files go to `/models/.partial-<job>/`. The model scanner skips hidden folders, so a half-downloaded model is never listed.
3. A dropped transfer resumes with an HTTP Range request. An attempt that receives no data for 2 minutes is retried, up to 10 times.
4. Each file must match the SHA-256 that Hugging Face records for it. A file that fails is deleted.
5. The folder is renamed to `/models/<variant>/`, with a `.sibyl-source.json` that records the repository, commit and digests. The API then regenerates the llama-swap configuration.

Jobs are saved in `/config/downloads.json` and resume after a restart. A Podman secret `hf-token`, if it exists, is sent to Hugging Face for gated repositories.

The fit estimate compares the file size with the VRAM of the selected GPUs, less 2 GiB per GPU for the KV cache. `split` means the model needs system RAM as well. Some models still run fully on GPU, because llama.cpp keeps a large token embedding table in system RAM; the model card says so.

### Model settings

Each model has typed settings in `models.yaml`. The System API checks every value, and the generator turns the settings into llama-server flags. The TUI and any other client edit these fields through `PUT /models/{id}`. They never edit flags.

| Setting | llama-server flag | Allowed values |
|---|---|---|
| `layout` | `--split-mode`, `--device` | `span` or `gpu:N` |
| `ctx_size` | `--ctx-size` | 0 (model default) or more |
| `sampling.temperature` | `--temp` | 0 to 5 |
| `sampling.top_k` | `--top-k` | 0 to 1000 |
| `sampling.top_p` | `--top-p` | 0 to 1 |
| `sampling.min_p` | `--min-p` | 0 to 1 |
| `sampling.repeat_penalty` | `--repeat-penalty` | 0 to 5 |
| `sampling.presence_penalty` | `--presence-penalty` | -2 to 2 |
| `speculative.types` | `--spec-type` | `none`, `draft-simple`, `draft-eagle3`, `draft-mtp`, `draft-dflash`, `draft-dspark`, `ngram-simple`, `ngram-map-k`, `ngram-map-k4v`, `ngram-mod`, `ngram-cache` |
| `speculative.draft_model` | `--spec-draft-model` | The ID of another installed model |
| `speculative.draft_max`, `draft_min` | `--spec-draft-n-max`, `--spec-draft-n-min` | 1 to 64, and 0 to 64 |
| `speculative.ngram_mod_min`, `ngram_mod_max` | `--spec-ngram-mod-n-min`, `--spec-ngram-mod-n-max` | 1 to 64 |
| `kv_cache.type_k`, `type_v` | `--cache-type-k`, `--cache-type-v` | `f32`, `f16`, `bf16`, `q8_0`, `q4_0`, `q4_1`, `iq4_nl`, `q5_0`, `q5_1` |

The flag names and the `--spec-type` values come from llama.cpp b11889. A llama.cpp upgrade must recheck them, because llama.cpp renames speculative flags between releases.

- Sampling settings are server defaults. A client can still override them per request through the OpenAI API.
- The types `draft-simple`, `draft-eagle3`, `draft-dflash` and `draft-dspark` need `draft_model`. `draft-mtp` uses the model's own multi-token prediction layer, and the `ngram-*` types need no second model.
- If a draft model is deleted, the generator turns speculative decoding off for the models that used it, and reports an issue. The model itself keeps running.
- Raw `args` cannot set a flag that has a typed setting, so each value has one source.

## Inference

### Images

`containers/llama-swap/Containerfile.hip` builds llama.cpp for `gfx1100` only, on `rocm/dev-ubuntu-24.04:<ROCm>-complete`. A single-target build compiles faster and the kernels match the card. The runtime stage keeps only the HIP runtime libraries that `llama-server` links against.

`containers/llama-swap/Containerfile.vulkan` builds llama.cpp with `GGML_VULKAN=ON` on Ubuntu 26.04. Mesa RADV is the Vulkan driver. The image has no ROCm userspace. It uses Ubuntu, not Wolfi, to match the upstream llama.cpp Vulkan build.

Both images add the llama-swap release binary and run as UID 10001 inside the container.

Build arguments:

| Argument | Default | Purpose |
|---|---|---|
| `LLAMA_CPP_REF` | pinned `bNNNNN` tag | llama.cpp release |
| `LLAMA_SWAP_VERSION` | pinned release | llama-swap release |
| `ROCM_VERSION` | `7.2.4` | ROCm base. Upstream llama.cpp tests 7.2.x. ROCm 7.14 is a benchmark candidate. |
| `GPU_TARGETS` | `gfx1100` | RDNA3 Navi 31 |
| `GGML_HIP_RCCL` | `OFF` | RCCL helps only `--split-mode tensor`. Upstream disables it by default. |

### Multi-GPU

llama-swap uses the `matrix` router. The matrix lists the model combinations that can run at the same time. Models pinned to different cards form one set, and a model that spans both cards forms a set on its own. When a request arrives, llama-swap evicts the models outside the cheapest set that contains the requested model. The System API generates the sets from the layout of each model.

The `capacity` router, which tracks VRAM per card, is on llama-swap `main` but not in release v266. A v266 binary rejects it with `unknown router "capacity" (valid: group, matrix)`. When a release includes it, the System API can switch to it.

| Layout | llama-server flags | Pools used |
|---|---|---|
| Span both cards | `--split-mode layer --device ROCm0,ROCm1` | `gpu0` and `gpu1` |
| Pin to card 0 | `--split-mode none --device ROCm0` | `gpu0` |
| Pin to card 1 | `--split-mode none --device ROCm1` | `gpu1` |
| Tensor parallel (experimental) | `--split-mode tensor -fa on -ctk f16 -ctv f16` | `gpu0` and `gpu1` |

`--split-mode tensor` is experimental upstream, and upstream gives no performance guarantee outside CUDA (`docs/multi-gpu.md` in llama.cpp). The benchmark decides if it beats `layer` on two 7900 XTX cards.

Device names differ per backend: `ROCm0` for HIP and `Vulkan0` for Vulkan. The System API writes the llama-swap configuration, so it fills in the right names.

### Benchmark

`scripts/bench.sh` runs `llama-bench` from both images against each installed model. It records prompt-processing and token-generation speed at 512, 8192 and 32768 tokens of context. The System API stores the faster backend per model and writes it into the llama-swap configuration.

### Embeddings for Open WebUI

Open WebUI uses `RAG_EMBEDDING_ENGINE=openai` against a llama-swap model that runs `llama-server --embedding`. That model sits in a persistent group so chat swaps do not unload it.

### Ollama-compatible shim

Many clients speak only the Ollama API, and llama-swap has none. The System API adds `/api/tags`, `/api/show`, `/api/chat`, `/api/generate`, `/api/pull` and `/api/ps`. Each route translates to the llama-swap OpenAI API or to the Hugging Face download path. Caddy routes `/api/*` on port 8443 to the shim, and `/v1/*` to llama-swap.

## Host image

`os/Containerfile` starts from `quay.io/fedora/fedora-bootc:44` and adds:

- The packages in `os/packages/common.txt` and `os/packages/<variant>.txt`.
- The `sibyl` user, its subordinate ID ranges, and lingering so its user manager starts at boot.
- Quadlet units, the Podman network, sysctl and nftables rules.
- The container signature policy in `/etc/containers/policy.json`.
- Host-only tools that cannot run in a container: GPU detection, the installer and disk management.

The AMD variant needs no extra kernel modules. `amdgpu` is in the Fedora kernel and is signed for Secure Boot. The host needs only the firmware (`amd-gpu-firmware`) and device access. The ROCm userspace is in the HIP image only.

The NVIDIA variant adds `akmod-nvidia` built at image build time and signed with a project MOK key. The user enrolls that key once with `mokutil`. The Intel variant adds `intel-compute-runtime` to the host for device nodes, and the inference image uses Vulkan or SYCL.

### GPU access from rootless containers

The `sibyl` user is in the `render` and `video` groups. Quadlet passes `/dev/kfd` and `/dev/dri` with `AddDevice=` and keeps the groups with `GroupAdd=keep-groups`. SELinux blocks device access by `container_t` unless the `container_use_devices` boolean is on. `sibyl-selinux.service` sets it at each boot.

## Storage

| Mode | Root | Models and data | Unlock |
|---|---|---|---|
| Live USB | Read-only image from the USB | Persistence partition on the USB, label `SIBYL_PERSIST`, mounted at `/var/lib/sibyl` | Passphrase at boot, optional |
| Installed | bootc on internal disk, A/B deployments | `/var` on the root disk, plus pools under `/var/mnt/models-N` | TPM2 through `systemd-cryptenroll`, passphrase as recovery |

The guided installer runs from the live USB. It wraps `bootc install to-disk` with these steps:

1. Pick a target disk. The installer lists disks with `lsblk -J` and excludes the boot USB.
2. Confirm the wipe. The installer prints the disk model, size and serial number first.
3. Create the LUKS2 root and install the image.
4. Enroll TPM2 with PCR 7 (Secure Boot state), and print a recovery key.
5. Copy models from the USB persistence partition, if the user asks.

Model storage pools are extra disks that the System API formats as LUKS2 plus XFS, with TPM2 unlock. Each pool mounts at `/var/mnt/models-N`. llama-swap sees all pools through one bind mount per pool. The TUI and System API show free space per pool.

The live USB is built with `bootc-image-builder --type iso`, plus a persistence partition that the first boot creates in the free space on the stick.

## Setup wizard

The setup wizard configures the hardware and the services. It runs in the TUI in three places: at the end of the disk installer, at the first boot of a live USB, and on demand with `sibyl setup`. Every step can run again later without a reinstall.

1. Detect the GPUs. The wizard reads `lspci` and `amd-smi` (or `nvidia-smi`), and lists each card with its model, VRAM, PCIe link width and speed.
2. Choose the GPUs for inference. The user can exclude a card, for example a card that drives a display.
3. Choose the default layout for the selected GPUs: span one model across all of them, or pin one model per card. A model can override the default later.
4. Choose the backend: HIP, Vulkan, or "benchmark". The benchmark runs a short `llama-bench` on both images with a small model and keeps the faster one.
5. Set the network: hostname, DHCP or a static address.
6. Set the Open WebUI admin account and import SSH public keys.
7. Show the API key and the TLS certificate fingerprint.
8. Optionally download a first model from the curated list, sized to the VRAM of the selected GPUs.

The wizard writes `/var/lib/sibyl/config/hardware.yaml`. The System API reads that file and generates three outputs:

- The llama-swap configuration, with device names and matrix sets for the chosen layout.
- The backend drop-in `llama-swap.container.d/backend.conf`, with the image and the GPU visibility variable (`HIP_VISIBLE_DEVICES` for HIP, `GGML_VK_VISIBLE_DEVICES` for Vulkan).
- The model size limits that the model picker uses.

When the GPU set changes, for example after a card is added, the System API detects the difference at boot and asks the user to run the GPU steps again.

## Security

- SELinux stays enforcing. Only Arcane runs with `SecurityLabelDisable=true`.
- Secure Boot uses the Fedora shim and kernel. The NVIDIA variant adds the project MOK key.
- CI signs every image with cosign keyless signing. `/etc/containers/policy.json` requires a signature from this repo's workflow for `ghcr.io/ihavespoons/sibyl-*`. Third-party images are pinned by digest in the Quadlet files.
- Every Quadlet container drops all capabilities, sets `NoNewPrivileges=true` and `ReadOnly=true`, and gets a tmpfs for `/tmp`. Writable state goes to named volumes only.
- nftables default-deny. Only 443, 8443, 9443 and rate-limited 22 are open.
- SSH accepts keys only. Root login is off.
- `bootc` and Podman auto-update are off by default. Updates are explicit, from the TUI, the System API, or `bootc upgrade`.

## Phases

| Phase | Scope | Exit check |
|---|---|---|
| 1 | Inference images, host Containerfile, Quadlets, Caddy image, CI build and signing | The AMD host image boots in a VM. llama-swap serves a GGUF model on the 7900 XTX. |
| 2 | System API and TUI in containers. They control units through the systemd D-Bus API. Setup wizard with GPU selection and layout. TUI editor for model settings: sampling, speculative decoding, KV cache, context and layout. Hugging Face picker. Ollama shim. Benchmark. | The API test suite passes against a running host. The wizard configures one card and two cards on the target host. |
| 3 | Live ISO, persistence, guided disk installer that ends in the setup wizard, LUKS with TPM2, model pools. Privileged host changes for the wizard (network, accounts, SSH keys) as allowlisted host agent actions | Install from USB to disk on the target host, and unlock without a passphrase. |
| 4 | NVIDIA and Intel variants, offline update bundles, signature policy enforced | Each variant boots. An unsigned image fails to pull. |
| 5 | User guide and developer guide | A new user installs Sibyl on a fresh host from the guide alone. |
| 6 | Smart model router: a gateway in front of llama-swap that serves local requests that fit and routes others to frontier providers (Anthropic, OpenAI, OpenRouter and similar), using request profiles and customisable rules, optionally decided by a small local classifier model | Design to be written when phases 2 to 5 are done |

## Open items

- Arcane Git Sync polls and has no webhooks. Compose files pulled from Git are read-only in the Arcane UI.
