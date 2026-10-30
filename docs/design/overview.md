# Sibyl design overview

This document gives contributors the design of Sibyl: what it is for, how its parts fit together, why each choice was made, and what is built. It records the design decisions from October 2026 and the delivery phases. Two parts have their own design documents: the [model router](router.md) and the [web console](web-console.md).

## Purpose and limits

Sibyl is an all-in-one inference server. It is a hardened, immutable Linux host that boots from USB or installs to disk. It serves OpenAI-, Anthropic- and Ollama-compatible APIs from llama.cpp, with a chat UI, a model manager and a container platform.

- Each GPU vendor has its own host image variant: `-amd`, `-nvidia` and `-intel`.
- The AMD variant is tested on a host with two Radeon RX 7900 XTX cards (the "test host" in this document).
- The NVIDIA and Intel variants build and pass the same checks, but no one has run them on hardware yet.
- Models are GGUF files only, quantized to fit consumer GPUs. Sibyl does not use vLLM.

## Decisions

| Area | Decision | Reason |
|---|---|---|
| Host OS | Fedora bootc (`quay.io/fedora/fedora-bootc:44`) | The OS is a Containerfile. Updates are atomic, with rollback. The kernel is current for new GPUs. |
| Container runtime | Podman with Quadlet, rootless | Fedora default. Quadlet units start at boot without a daemon. |
| Container UI | Arcane (BSD-3-Clause) | Compose editor, Git Sync, native Podman and Quadlet pages. Dockhand is source-available (BSL 1.1), so it is out. |
| Inference engine | llama.cpp `llama-server` behind llama-swap | One engine for every GPU vendor, with fast single-stream GGUF inference. llama-swap swaps models and places them on GPUs. |
| GPU backends | HIP (ROCm) and Vulkan for AMD, CUDA and Vulkan for NVIDIA, Vulkan for Intel | The fastest backend differs by GPU, quant and context size. The benchmark compares the variant's backends. |
| Model format | GGUF only | Quantized models fit consumer GPUs. No vLLM. |
| Multi-GPU | Per-model layout through the llama-swap matrix router | One model can span both cards, or two models can run on one card each. |
| GPU vendors | Separate image variants: `-amd`, `-nvidia`, `-intel` | Only the NVIDIA variant carries out-of-tree kernel modules, signed with the project MOK key. NVIDIA and Intel are untested on hardware. |
| Base for service images | Wolfi where possible | Smaller images with fewer CVEs. The inference images use the vendors' bases, because Wolfi does not package ROCm or CUDA. |
| Boot | Live USB with persistence, and install to disk | Try it from USB, then install. |
| Disks | Guided installer, LUKS with TPM2 unlock, model storage pools, USB persistence | See [Storage](#storage). |
| Hardening | SELinux enforcing, Secure Boot, signed images, CIS-style lockdown | See [Security](#security). |
| Updates | GHCR registry, plus offline bundles | `bootc upgrade` online. `bootc switch` to an OCI archive offline. |
| CI | GitHub Actions, SHA-pinned actions, cosign keyless and key signing | Images and ISOs build in CI. The host verifies the key signature. |
| Model download | Hugging Face GGUF picker in the System API and TUI, plus an Ollama-compatible API shim | Clients written for Ollama work unchanged. |

## Architecture

### Repository layout

```
os/
  Containerfile              # bootc host image, ARG GPU_VARIANT=amd|nvidia|intel
  packages/                  # dnf package lists per variant
  rootfs/                    # files copied into / of the host image
containers/
  llama-swap/                # Containerfile.hip, Containerfile.vulkan
  llama-swap-cuda/           # Containerfile (from llama.cpp's CUDA image)
  caddy/                     # Wolfi-based TLS front door
  system-api/                # Wolfi-based management API
  tui/                       # Wolfi-based console TUI and setup wizard
  live/                      # live boot layer for the ISO
cmd/, internal/              # Go services: System API, router, TUI, hostd, installer, update, eval
scripts/                     # local build helpers, ISO creation, benchmark
docs/                        # documentation
.github/workflows/bootc.yml  # build, sign, push
```

### Runtime layout

All Sibyl containers run rootless under one system user, `sibyl` (UID 1001). Rootless Podman confines each container to that user's subordinate UID range. A container escape gives an unprivileged user, not root.

systemd owns the core services. The Quadlet files are in the OS image under `/etc/containers/systemd/users/1001/`. Arcane can stop a core container, but the next boot or `systemctl --user restart` brings it back from the image definition. User stacks that Arcane creates live under `/var/lib/sibyl/stacks`.

Arcane uses the same user's Podman socket, so it sees and controls the core containers. It runs with `SecurityLabelDisable=true`, because the Arcane docs require it on SELinux hosts. The socket proxy from the earlier design is not used. A proxy cannot filter the Quadlet and D-Bus calls that Arcane needs, and rootless Podman already removes root from the blast radius.

| Container | Image | Port on the `sibyl` network | Exposed on the host |
|---|---|---|---|
| `llama-swap` | `ghcr.io/diffsec/sibyl-llama-swap:<ver>-hip`, `-vulkan` or `-cuda` | 8080 | No |
| `router` | `ghcr.io/diffsec/sibyl-router` | 8080 | No |
| `open-webui` | `ghcr.io/open-webui/open-webui` (pinned digest) | 8080 | No |
| `system-api` | `ghcr.io/diffsec/sibyl-system-api` | 3001 | No |
| `arcane` | `ghcr.io/getarcaneapp/manager` (pinned digest) | 3552 | No |
| `caddy` | `ghcr.io/diffsec/sibyl-caddy` | 10443, 18443, 19443 | Yes, through nftables redirect |

The TUI is not a service. Each operator starts it on demand in their own rootless Podman (see [Consoles](#consoles)).

### Request flow

nftables redirects three host ports to Caddy:

| Host port | Caddy port | Serves |
|---|---|---|
| 443 | 10443 | Open WebUI |
| 8443 | 18443 | The router's OpenAI and Anthropic API (`/v1/*`), the Ollama shim (`/api/*`), the System API (`/system/*`) and the web console (`/console/*`) |
| 9443 | 19443 | Arcane |

The input chain accepts the high ports only for redirected connections. This keeps `net.ipv4.ip_unprivileged_port_start` at its default of 1024.

Caddy sends `/v1/*` to the model router. The router checks each client's key and policy, and then forwards the request to llama-swap or to a provider. Open WebUI and the Ollama shim also call the router. The [router design](router.md) describes it.

## Components

### Host image

`os/Containerfile` starts from `quay.io/fedora/fedora-bootc:44` and adds:

- The packages in `os/packages/common.txt` and `os/packages/<variant>.txt`.
- The `sibyl` user, its subordinate ID ranges, and lingering so its user manager starts at boot.
- Quadlet units, the Podman network, sysctl and nftables rules.
- The container signature policy in `/etc/containers/policy.json`.
- Host-only tools that cannot run in a container: GPU detection, the installer and disk management.

The AMD variant needs no extra kernel modules. `amdgpu` is in the Fedora kernel and is signed for Secure Boot. The host needs only the firmware (`amd-gpu-firmware`) and device access. The ROCm userspace is in the HIP image only.

The Intel variant also uses in-kernel drivers (`i915` and `xe`) and only adds the firmware (`intel-gpu-firmware`). It adds `intel-compute-runtime` to the host for device nodes. Its inference image is the Vulkan one, with Mesa ANV. SYCL was named as a second choice and is not built.

The NVIDIA variant runs `os/variants/nvidia.sh` during the image build:

1. It adds RPM Fusion and the NVIDIA Container Toolkit repository.
2. It builds the driver's open kernel modules (`akmod-nvidia`) with `akmods` for the image's kernel. NVIDIA recommends the open modules for Turing and newer GPUs.
3. akmods signs the modules with the project MOK key. CI passes the key as the build secret `mok_key` (the `MOK_PRIVATE_KEY` repository secret). The key exists only inside that one build step, so no image layer holds it. The public certificate is in the image at `/usr/share/sibyl/sibyl-mok.der`.
4. It enables `nvidia-cdi-refresh`, which writes the CDI devices at boot. It sets kernel arguments in `/usr/lib/bootc/kargs.d` that keep `nouveau` off the GPU.

Secure Boot loads the NVIDIA modules only after the host enrolls the MOK certificate once with `mokutil`. [First boot](../getting-started/first-boot.md) gives the steps. The model fit estimate shows `unknown` on NVIDIA and Intel, because their drivers do not report VRAM in sysfs the way `amdgpu` does.

CI builds the NVIDIA and Intel host images only for release tags or with `gh workflow run bootc.yml -f variants=true`, to save runner minutes. The CUDA inference image builds with the others.

#### GPU access from rootless containers

The `sibyl` user is in the `render` and `video` groups. Quadlet passes `/dev/kfd` and `/dev/dri` with `AddDevice=` and keeps the groups with `GroupAdd=keep-groups`. SELinux blocks device access by `container_t` unless the `container_use_devices` boolean is on. `sibyl-selinux.service` sets it at each boot.

### Inference images

`containers/llama-swap/Containerfile.hip` builds llama.cpp on `rocm/dev-ubuntu-24.04:<ROCm>-complete`. It builds for the Radeon and Ryzen targets that llama.cpp's own ROCm image builds, without the Instinct cards. The runtime stage keeps only the HIP runtime libraries that `llama-server` links against, and the rocBLAS and hipBLASLt kernel files for those targets. The build fails if ROCm has no rocBLAS kernels for a target, because else rocBLAS fails at run time on that card. One target took about 15 minutes to compile on a GitHub runner.

`hardware.HIPTargets` holds the same target list, and a test keeps it in step with `GPU_TARGETS`. `gpu.Detect` reads each AMD card's target from the amdkfd topology in sysfs (`gfx_target_version`, matched to the card by its render node). `PUT /hardware` refuses HIP for a card outside the list. With no `hardware.yaml`, the default backend is Vulkan when GPU 0 is outside the list. The benchmark runs only the backends that every selected card supports. A card whose target is unknown counts as supported.

`containers/llama-swap/Containerfile.vulkan` builds llama.cpp with `GGML_VULKAN=ON` on Ubuntu 26.04. Mesa is the Vulkan driver for AMD (RADV) and Intel (ANV). The image has no ROCm userspace. It uses Ubuntu, not Wolfi, to match the upstream llama.cpp Vulkan build.

`containers/llama-swap-cuda/Containerfile` starts from llama.cpp's own CUDA image (`ghcr.io/ggml-org/llama.cpp:full-cuda-<release>`, pinned by digest), so CI compiles no CUDA. It moves llama.cpp to `/opt/llama`, like the other images. The NVIDIA driver and `libcuda` come from the host through CDI, so the library check allows only `libcuda.so.1` to be missing at build time.

All the images add the llama-swap release binary and run as UID 10001 inside the container.

| Build argument | Default | Purpose |
|---|---|---|
| `LLAMA_CPP_REF` | pinned `bNNNNN` tag | llama.cpp release |
| `LLAMA_SWAP_VERSION` | pinned release | llama-swap release |
| `GPU_TARGETS` | `gfx1030;gfx1100;gfx1101;gfx1102;gfx1150;gfx1151;gfx1200;gfx1201` | RDNA2 (RX 6800 and 6900, PRO W6800), RDNA3 (RX 7000, PRO W7000), RDNA3.5 (Ryzen AI 300, Strix Halo), RDNA4 (RX 9000, AI PRO R9700) |
| `GGML_HIP_RCCL` | `OFF` | RCCL helps only `--split-mode tensor`. Upstream disables it by default. |

The base images are plain `FROM` lines, so Dependabot can update them. The HIP build uses `docker.io/rocm/dev-ubuntu-24.04:7.2.4-complete`. Upstream llama.cpp tests ROCm 7.2.x, and ROCm 7.14 is a benchmark candidate. Dependabot leaves the ROCm and Ubuntu releases alone, because a new release changes the llama.cpp build. A maintainer moves them by hand after a benchmark.

### Multi-GPU

llama-swap uses the `matrix` router. The matrix lists the model combinations that can run at the same time. Models pinned to different cards form one set, and a model that spans both cards forms a set on its own. When a request arrives, llama-swap evicts the models outside the cheapest set that contains the requested model. The System API generates the sets from the layout of each model.

The `capacity` router, which tracks VRAM per card, is on llama-swap `main` but not in release v266. A v266 binary rejects it with `unknown router "capacity" (valid: group, matrix)`. When a release includes it, the System API can switch to it.

| Layout | llama-server flags | Pools used |
|---|---|---|
| Span both cards | `--split-mode layer --device ROCm0,ROCm1` | `gpu0` and `gpu1` |
| Pin to card 0 | `--split-mode none --device ROCm0` | `gpu0` |
| Pin to card 1 | `--split-mode none --device ROCm1` | `gpu1` |
| Tensor parallel (experimental) | `--split-mode tensor -fa on -ctk f16 -ctv f16` | `gpu0` and `gpu1` |

The table uses HIP device names, from the AMD test host. The other backends work the same way. Device names differ per backend: `ROCm0` for HIP, `CUDA0` for CUDA and `Vulkan0` for Vulkan. The System API writes the llama-swap configuration, so it fills in the right names.

The System API also writes the backend drop-in (`internal/llamaswap` `DropIn`). The drop-in picks the image, the GPU devices and the variable that limits the visible GPUs:

| Backend | Devices | Visibility variable |
|---|---|---|
| HIP | `/dev/kfd` and `/dev/dri` | `HIP_VISIBLE_DEVICES` |
| Vulkan (AMD and Intel) | `/dev/dri` | `GGML_VK_VISIBLE_DEVICES` |
| CUDA | CDI device `nvidia.com/gpu=all` | `CUDA_VISIBLE_DEVICES` |

The System API learns the variant from `SIBYL_GPU_VARIANT`, and accepts only that variant's backends.

`--split-mode tensor` is experimental upstream, and upstream gives no performance guarantee outside CUDA (`docs/multi-gpu.md` in llama.cpp). The benchmark decides if it beats `layer` on two 7900 XTX cards.

### System API

The System API is a Go service (`cmd/sibyl-api`, `internal/`). It runs in a container with no access to the systemd bus, so SELinux confinement stays on. It reads and writes only these directories:

| Host path | In the container | Content |
|---|---|---|
| `/var/lib/sibyl/config` | `/config` | `hardware.yaml`, `models.yaml`, `llama-swap/config.yaml` |
| `/var/lib/sibyl/models` | `/models` | GGUF files |
| `/var/lib/sibyl/run` | `/run/sibyl` | Request queue and `status.json` |
| `/var/lib/sibyl/home/.config/containers/systemd/llama-swap.container.d` | `/quadlet` | `backend.conf`, which overrides the image default |

Each change to the hardware or to model settings runs an apply. The apply regenerates the llama-swap configuration, and llama-swap reloads it without a restart. llama-swap polls the file with `stat`, so the configuration is mounted as a directory. A single-file bind mount keeps the old inode after an atomic rename. If the backend or the GPU set changes, the apply also rewrites `backend.conf` and queues a reload and a restart.

Extra llama-server arguments must match `^[A-Za-z0-9_.,:=/+-]+$`. Flags that Sibyl sets, such as `--model`, `--port` and `--device`, are rejected.

The [System API reference](../reference/system-api.md) lists every route.

### Host agent

The host agent (`/usr/libexec/sibyl/agent`) is the only path from a container to systemd. It runs as the `sibyl` user.

1. The System API creates an empty file named `<timestamp>.<action>.<unit>` in the request queue.
2. The `sibyl-agent.path` user unit starts the agent.
3. The agent accepts `restart` for one of the six Sibyl units (`llama-swap`, `router`, `open-webui`, `caddy`, `arcane`, `system-api`), `reload.all` (a `systemctl --user daemon-reload`), and `bench.<model>` (see [Benchmark](#benchmark)). It deletes every request.

The `sibyl-status.timer` writes the unit states to `status.json` every 15 seconds.

### Privileged host actions

The setup wizard changes the hostname, the network and the login accounts. Model pools and host updates also need root. The System API runs as `sibyl`, so a second agent, `sibyl-hostd` (`cmd/sibyl-hostd`, `internal/hostd`), runs as root and does these actions.

1. The System API writes one JSON request per file into `/var/lib/sibyl/run/host`. It checks the request with the same rules as `sibyl-hostd` first, and returns 422 for a bad request.
2. `sibyl-hostd.path` starts `sibyl-hostd.service`. The agent reads each request without following symlinks, deletes it, and checks it again. Only the agent's checks protect the host.
3. The agent runs fixed commands with arguments, never a shell string. The account and network actions use `hostnamectl`, `nmcli`, `busctl`, `useradd`, `usermod`, `userdel`, `runuser` and `restorecon`. The pool, update and password actions use commands such as `cryptsetup`, `systemd-cryptenroll`, `mkfs.xfs`, `mount`, `umount`, `systemctl` and `unix_chkpwd`.
4. It writes a result to `/var/lib/sibyl-host/out/results/<id>.json` and the host state to `/var/lib/sibyl-host/out/state.json`. The System API mounts that directory read-only, and waits up to 45 seconds for the result. A request that takes longer returns an ID, and the System API reports the result later.

The rules keep root out of reach of the System API. Any process that runs as `sibyl` can queue requests, and that includes Arcane and the containers it starts.

- Accounts go into `sibyl-operators` only, never into `wheel`. An operator can run `sibyl`, but not `sudo`.
- The agent changes and deletes only the accounts that it created. It records them in `/var/lib/sibyl-host/managed-users.json`, which only root can read. `root`, `admin`, `sibyl` and other system names are refused.
- The agent refuses to change or delete an account in `wheel`, even one on its managed list.
- SSH keys must be `ssh-ed25519`, ECDSA, security-key types, or RSA of 2048 bits or more. Key options such as `command=` are refused. The account itself writes its `authorized_keys` through `runuser`, so a symlink in its home cannot redirect a write by root.
- `PUT /host/users/{name}` can also take keys from `https://github.com/<user>.keys`.
- Deleting an operator also deletes its home directory.

A network change runs inside a NetworkManager checkpoint. If no confirm arrives in 120 seconds (30 to 600 can be set), NetworkManager restores the old configuration by itself. A wrong address therefore cannot lock out a remote user. The user reconnects at the new address and confirms the change (`POST /host/network/confirm`). Only IPv4 on ethernet interfaces is supported. The NetworkManager dispatcher then starts `sibyl-tls.service`, which issues a new server certificate for the new address (see [Security](#security)).

`sibyl-hostd-state.timer` rewrites the state every 5 minutes, so accounts added by hand also show.

The web console's password check uses a second, in-memory queue of `sibyl-hostd`. The [web console design](web-console.md#the-password-check) describes it.

### Online updates

A host updates from the registry through three `sibyl-hostd` actions. The `sibyl-hostd` unit has no network, so each action starts a unit that runs bootc:

| Unit | Command |
|---|---|
| `sibyl-upgrade-check.service` | `bootc upgrade --check` |
| `sibyl-upgrade.service` | `bootc upgrade` |
| `sibyl-upgrade-reboot.service` | `bootc upgrade --apply` |

Each unit rewrites the host state when it ends. The state holds the booted, staged and available versions from `bootc status`, and the state of each unit. The update keeps the image and the signature policy of the booted deployment, and the request cannot name another image. The TUI Status screen and the web console Status page have the same controls. [Updates](../guides/updates.md) tells the owner how to use them.

### Model downloads

The download manager (`internal/download`) runs one job at a time. Each job pins a commit, so every file comes from one revision even if the repository changes during the download.

1. The job checks that the disk has room for the rest of the download plus 2 GiB.
2. Files go to `/models/.partial-<job>/`. The model scanner skips hidden folders, so a half-downloaded model is never listed.
3. A dropped transfer resumes with an HTTP Range request. An attempt that receives no data for 2 minutes is retried, up to 10 times.
4. Each file must match the SHA-256 that Hugging Face records for it. A file that fails is deleted.
5. The folder is renamed to `/models/<variant>/`, with a `.sibyl-source.json` that records the repository, commit and digests. The API then regenerates the llama-swap configuration.

Jobs are saved in `/config/downloads.json` and resume after a restart.

Gated and private repositories need a Hugging Face access token. The System API checks the token with Hugging Face's `/api/whoami-v2`. It stores the token in `/config/hf-token` with mode 0600, and the account name in `/config/huggingface.json`. Searches, repository listings and downloads use it at once, with no restart. A gated model also needs its terms accepted on huggingface.co with the same account. When Hugging Face refuses a request, the error says so. The download then fails at once instead of retrying, so a retry resumes it after the fix. [Models](../guides/models.md) tells the owner how to sign in.

The fit estimate compares the file size with the VRAM of the selected GPUs, less 2 GiB per GPU for the KV cache. `split` means the model needs system RAM as well. Some models still run fully on GPU, because llama.cpp keeps a large token embedding table in system RAM. The model card says so.

### Model settings

Each model has typed settings in `models.yaml`. The System API checks every value, and the generator turns the settings into llama-server flags. The TUI, the web console and any other client edit these fields through `PUT /models/{id}`. They never edit flags. The [configuration reference](../reference/configuration.md) lists every field.

| Setting | llama-server flag | Allowed values |
|---|---|---|
| `layout` | `--split-mode`, `--device` | `span` or `gpu:N` |
| `ctx_size` | `--ctx-size` | 0 (Sibyl fits it to the GPUs) or more |
| `fit` | `--ctx-size`, `--cache-type-k`, `--cache-type-v` | `quality` (default) or `context` |
| `preset` | `--temp`, `--top-k`, `--top-p`, `--min-p` | `general`, `coding`, `agentic`, `writing` |
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
| `moe.cpu_layers` | `--n-cpu-moe`, or `--cpu-moe` for -1 | -1 (all layers), 0 (off), or 1 to 4096 |
| `moe.cache_mib` | `--moe-cache-mib` | 0 (off) to 1,048,576, and only with `moe.cpu_layers` |
| `parallel` | `--parallel` | See [Context size and memory estimate](#context-size-and-memory-estimate) |
| `tensor_split` | `--tensor-split` | The share of the layers on each GPU of a spanning model |
| `reasoning` | `--chat-template-kwargs` | A value of the variable that the model's chat template reads |

The flag names and the `--spec-type` values come from llama.cpp b11889. A llama.cpp upgrade must recheck them, because llama.cpp renames speculative flags between releases.

- Sampling settings are server defaults. A client can still override them per request through the OpenAI API.
- The types `draft-simple`, `draft-eagle3`, `draft-dflash` and `draft-dspark` need `draft_model`. `draft-mtp` uses the model's own multi-token prediction layer, and the `ngram-*` types need no second model.
- If a draft model is deleted, the generator turns speculative decoding off for the models that used it, and reports an issue. The model itself keeps running.
- Raw `args` cannot set a flag that has a typed setting, so each value has one source.
- The sampling fields show the model file's recommendation, or llama.cpp's default, as their placeholder. llama-server applies the file's values when a setting is empty. `/props` on the test host reported the file's temperature 1.0 and top-k 20.
- `reasoning` follows the model's chat template. The variable and its values differ by model family, so Sibyl reads them from the template in the GGUF file (`internal/models/reasoning.go`). An empty setting leaves the template's default.

### Context size and memory estimate

`GET /models` includes each model's GGUF metadata (`internal/gguf`, `models.ReadMeta`): the architecture, the native context, the per-token KV cache elements, and the sampling defaults in `general.sampling.*`. The System API caches it by file size and modification time. The reader reads only the header, and refuses lengths over fixed limits.

#### The automatic context

With no `ctx_size`, Sibyl fits the context to the GPUs that the model's layout uses (`models.FitContext`, called from the apply).

- The budget is 92% of their VRAM less the weights on the GPUs.
- The weights on the GPUs are the tensors of every shard except the input layer (`token_embd`, `per_layer_token_embd`, `position_embd`, `token_types`), because llama.cpp keeps the input layer in system RAM. A model with no `output.weight` gets a GPU copy of `token_embd`, so that copy counts.
- The sizes come from the tensor offsets in the GGUF header (`gguf.Tensors`). When a shard cannot be read, the fit uses the file size.
- With `fit: quality`, the fit keeps the KV cache types (f16 by default). It takes the native context if it fits, else the largest multiple of 4,096 tokens that fits.
- With `fit: context` and no cache types set, it tries f16, then `q8_0`, then `q4_0`. It takes the first that reaches the native context, or else the one that gives the most tokens.
- A `ctx_size` or cache type that the owner sets is kept.

The chosen values go into the llama-swap command line and the router's `models.json`, and `GET /models` reports them as `fit`. `models.yaml` keeps only the owner's choices, so a GPU change refits. Before 2026-10-29, an empty `ctx_size` meant the native context. That is often 262,144 tokens, and it often spilled into system RAM.

#### Embedding models

Embedding models stay loaded next to every chat model (see [Multi-GPU](#multi-gpu)), so the fit reserves their memory first (`effectiveAll` in `internal/api`). An embedding model with no `ctx_size` gets 8,192 tokens (`models.EmbeddingContext`) instead of its native context. On the test host, `qwen3-embedding-0.6b-q8_0` used 4.8 GB of VRAM at its native 32,768 tokens. The estimate of weights plus 32,768 tokens of context gave 4.6 GB.

The fit subtracts each embedding model's weights and context from the budget of its GPU. A model that spans GPUs then gets a `--tensor-split` in proportion to what each GPU has left, in MiB (`models.SplitFor`). Without a fixed split, llama.cpp splits by the free memory at load time. If the chat model loads before the embedding model, the split is even and the embedding model's GPU overflows. On 2026-10-30, flint-q8_0 failed to load at 135,168 tokens with `cudaMalloc failed: out of memory` on GPU 0, because the fit did not count the 4.8 GB on GPU 1. An owner's `tensor_split` is kept.

#### Parallel slots

`parallel` sets llama-server's `--parallel`: the number of requests that it answers at once. The slots share one KV cache, so `--ctx-size` is the context of all slots together. With `parallel` set and no `ctx_size`, the fit aims for the native context for each slot, and else the largest multiple of 4,096 tokens per slot that fits. `GET /models` reports `slots` and `slot_ctx`. The router's `models.json` gets `slot_ctx` as the local context, because a request is sure of only its share when every slot is busy. With no `parallel`, llama-server picks its own slot count, and the router uses the whole context.

#### The memory estimate

The TUI's context field shows what Sibyl chooses and an estimate of the GPU memory for the context and KV cache types. It says when the total passes 92% of the VRAM of the GPUs that the model's layout uses. llama.cpp then keeps the rest in system RAM.

On the test host, the 63.3 GB FN-R1 quant of Qwen3.8-Flash-Next REAP-288 runs fully on two 24 GB GPUs at 262,144 tokens with a `q8_0` cache, and uses 44.5 GB of VRAM. Its `per_layer_token_embd` table is 26.8 GiB and stays in system RAM, so 31.6 GiB of weights go to the GPUs.

The estimate per token is the sum of:

- the K and V elements of every layer that keeps a full-length cache, at the cache type's size (recurrent layers, sliding-window layers and multi-token prediction layers are left out),
- the sparse attention indexer, if the model has one, in f16,
- 8 KiB of compute buffer,
- for a quantized cache, 4 bytes for each K and V element of the largest layer.

These terms come from measurements on the test host (llama.cpp b11889, ROCm): the VRAM change between a 32,768 and a 131,072 token context. The estimate matched six of seven measurements within 0.4%, across a dense model, a hybrid model, and a sliding-window model at f16, q8_0 and q4_0. It was 3% low on a model with compressed attention layers. `internal/models/meta_test.go` holds the measured values.

### Mixture of Experts offload

A Mixture of Experts (MoE) model routes each token through a few of its experts, so most expert weights sit idle at any time. The `moe` settings keep the expert weights in system RAM and give the GPUs the rest of the model. A MoE model that is larger than the VRAM then runs with its attention layers and KV cache on the GPUs. The setting names and flags come from llama.cpp b11889 (`common/arg.cpp`).

- `moe.cpu_layers` keeps the routed experts of the first N layers in system RAM, and -1 keeps all of them there. The expert tensors are those that match `blk.N.ffn_(up|down|gate|gate_up)_(ch|)exps`. Shared experts stay on the GPUs.
- `moe.cache_mib` is a GPU cache for the experts in system RAM. llama.cpp copies each expert that a token picks into the cache, and keeps the most used ones there (`src/llama-moe-cache.cpp`, a segmented LRU). An expert that is in the cache runs on the GPU. One that is not runs on the CPU. With two GPUs, llama.cpp splits the cache like the layers.

The experts in system RAM stay memory-mapped from the model file. llama.cpp reads the whole file once at load, but the pages are clean, so the kernel can drop them. When RAM is short, the kernel drops the experts that go unused, and reads an expert back from disk when the router picks it again. A model larger than the RAM then still runs, at the speed of the disk for the experts that miss. A `--load-mode` of `mlock` or `none` in `args` turns this off. Every expert then stays in RAM.

The context fit counts the moved experts out of the GPU weights, and counts the cache in (`models.Meta.VRAMWeights`). The cache counts up to the size of the experts in system RAM, because llama.cpp gives it no more slots than that. A larger cache therefore gives a shorter automatic context. `GET /models` reports each model's `expert_count` and the expert bytes of each layer (`expert_layer_bytes`).

The System API refuses `moe` settings for a model file with no `expert_count`, because llama.cpp fails to load a dense model with a MoE cache. The TUI shows the Mixture of Experts group only for a MoE model, and its description gives the size of the experts that stay in system RAM. llama.cpp turns off pipeline parallelism with a MoE cache, and refuses a cache with `--split-mode tensor`, which Sibyl does not use.

### Benchmark

`POST /models/{id}/benchmark` compares the backends on one model.

1. The host agent unloads llama-swap's models.
2. It runs `llama-bench` on the selected GPUs, in the image of each backend that the variant ships and that every selected GPU supports.
3. Each run uses 512 prompt tokens and 128 generated tokens, twice each.
4. `GET /models/{id}/benchmark` and the model list report tokens per second for each backend, and recommend the faster one by generation speed.

llama-swap runs one image, so the backend applies to every model. The recommendation is advice: the owner sets the backend on the Hardware screen.

### Embeddings for Open WebUI

Open WebUI's document search uses an embedding model from llama-swap through its OpenAI engine. The matrix router keeps the embedding model loaded beside a chat model.

Open WebUI stores its embedding settings in its database (`rag.embedding_model` is persistent configuration in v0.12.4), so environment variables apply on the first start only. Sibyl sets them through Open WebUI's admin API instead:

1. The admin gives the System API an Open WebUI admin login or an API key (`POST /integrations/open-webui/connect` or `PUT /integrations/open-webui`). On a fresh install, the login creates the first admin.
2. With a login, the System API signs in, or signs up when no account exists, which Open WebUI makes the admin. It turns on API keys in the admin config if they are off, and reads the admin's API key. It creates a key only when the account has none, because creating one replaces the old key. The password is used for that request only and is never stored.
3. The System API checks the key against an admin-only endpoint, then stores it in `/config/open-webui.key` with mode 0600.
4. After each configuration change, and at startup, the System API points Open WebUI at the first installed model marked as an embedding model, through `POST /api/v1/retrieval/embedding/update`. It changes nothing when Open WebUI already uses that model.

### Ollama-compatible API

Many clients speak only the Ollama API, and llama-swap has none. The System API serves `/api/version`, `/api/tags`, `/api/show`, `/api/ps`, `/api/chat`, `/api/generate`, `/api/embed` and `/api/embeddings` (`internal/ollama`). Each call translates to the OpenAI API and goes to the router.

Caddy routes `/api/*` on port 8443 to the System API. The shim checks the caller's key with the router (`GET /v1/models`, kept for a minute) and sends it on, so the caller's router policy applies. The shim refuses the System API key.

- Streaming turns OpenAI server-sent events into Ollama's NDJSON frames, with `thinking` from llama-server's `reasoning_content`.
- Tool calls convert in both directions. Ollama sends arguments as a JSON object, and OpenAI as a JSON string.
- Ollama `options` map to OpenAI and llama-server fields: `temperature`, `top_p`, `top_k`, `min_p`, `repeat_penalty`, `presence_penalty`, `seed`, `num_predict`, `stop`. `format` maps to JSON mode or a JSON schema.
- `/api/pull`, `/api/create` and the other model management calls return 501, because Sibyl downloads models through its own picker.

### Model router

The router (`cmd/sibyl-router`, `internal/router`) is a gateway in front of llama-swap. It owns the inference keys, applies each client's policy and the owner's rules, and sends a request to a local model or to a frontier provider. The [router design](router.md) covers it in full.

### Consoles

Sibyl has two consoles. Both talk only to the System API, so neither has host privileges.

The TUI (`cmd/sibyl-tui`, `internal/tui`) is a container image, `sibyl-tui`. Members of the `sibyl-operators` group run it with `sibyl`, with no sudo. The command starts the TUI in the caller's own rootless Podman, read-only with all capabilities dropped. The TUI connects through Caddy on `127.0.0.1:18443` and verifies the host certificate as `sibyl.local`.

| Screen | Purpose |
|---|---|
| Status | GPUs with VRAM in use, service states with restarts, and host updates |
| Hardware | The GPU part of the setup wizard: GPUs for inference, backend, default layout |
| Models | Installed models, the settings editor, the benchmark, and delete |
| Download | Hugging Face search, variants with fit, confirmation, and download jobs |
| Host | Hostname, network with rollback, model pools, and operator accounts with SSH keys |
| Router | Router clients, providers, profiles and rules, calibration, and the last decisions |

At each boot, `sibyl-operator-export.service` copies the System API key, the `default` inference key and the CA certificate to `/var/lib/sibyl-operator/`, readable by `sibyl-operators` only. Membership gives full control of the System API, so only trusted users belong in the group. [Host](../guides/host.md) tells the owner how to add an operator.

The web console is a browser view of the same screens, served by the System API at `/console/`. The [web console design](web-console.md) covers it. [Consoles](../guides/consoles.md) is the user guide for both.

### Setup wizard

The setup wizard configures the hardware and the services. It runs in the TUI with `sibyl setup`, and the web console has a Setup page with the same steps. The installer's last message and the live menu both point to `sibyl setup`. Every step can run again later without a reinstall.

`sibyl setup` starts the TUI in wizard mode (`internal/tui/wizard.go`). Each step opens the normal screen for it and says which key to press. `]` and `[` move between steps.

| Step | Screen | What it sets |
|---|---|---|
| 1. GPUs, backend and layout | Hardware | The GPUs for inference (a card that drives a display can be left out), the backend, and the default layout: span one model across all GPUs, or pin one model per card. A model can override the layout later. |
| 2. Hostname and network | Host | The hostname, and DHCP or a static address |
| 3. Operator accounts and SSH keys | Host | Operators who can run `sibyl` over SSH |
| 4. Open WebUI administrator | Status | The first Open WebUI admin |
| 5. First model | Download | A first model, with the fit for the selected GPUs |
| 6. API keys for your programs | Router | One router client key for each program |
| Done | Status | The client URLs, where the keys are, and the SHA-256 fingerprint of the Sibyl CA certificate |

The GPU detection reads sysfs and lists each card with its vendor, ROCm target on AMD, VRAM, and PCIe link width. To pick the backend by measurement, the owner runs the benchmark from the Models screen. It needs an installed model.

The Hardware step writes `/var/lib/sibyl/config/hardware.yaml`. The System API reads that file and generates three outputs:

- The llama-swap configuration, with device names and matrix sets for the chosen layout.
- The backend drop-in `llama-swap.container.d/backend.conf`, with the image and the GPU visibility variable (see [Multi-GPU](#multi-gpu)).
- The model size limits that the model picker uses.

[First boot](../getting-started/first-boot.md) walks the owner through the wizard.

## Storage

| Mode | Root | Models and data | Unlock |
|---|---|---|---|
| Live USB | Read-only squashfs on the USB, with a RAM overlay | Persistence partition on the USB, XFS label `SIBYLPERSIST`, mounted at `/var/lib/sibyl` | None |
| Installed | bootc on internal disk, A/B deployments | `/var` on the root disk, plus pools under `/var/lib/sibyl-pools/pool-N` | TPM2 with PCR 7, and a recovery key |

### Live USB

`containers/live/Containerfile` adds a live boot to the host image: `dmsquash-live` in the initramfs, shim and GRUB for an ISO, and `iso.yaml`. CI turns that image into an ISO with image-builder's `generic-iso` type. The ISO also holds the host image in its container storage (`--bootc-installer-payload-ref`), so the installer works offline. CI pushes the ISO to `ghcr.io/diffsec/sibyl-iso:<tag>-<variant>` as a signed OCI artifact, because it is larger than the 2 GiB limit for release assets. [Install](../getting-started/install.md) tells the owner how to get it.

- image-builder boots the live system with `enforcing=0`, because the squashfs has no SELinux labels. The installed system runs enforcing.
- The first console logs in as root and shows a menu: install, make a persistence partition, shell, reboot.
- Without persistence, the Sibyl services do not start, because their images do not fit in RAM. `sibyl-install persist` adds an XFS partition in the free space after the ISO on the USB. At the next boot it mounts at `/var/lib/sibyl`, and the services start from it.

### Guided installer

`sibyl-install` (`cmd/sibyl-install`, `internal/install`) runs as root from the live menu. bootc's own `--block-setup tpm2-luks` keeps no recovery key. The installer therefore sets up LUKS itself and then runs `bootc install to-filesystem`, as the bootc documentation recommends.

1. The user picks a disk. The installer lists disks with `lsblk -J`, and refuses the boot USB, disks under 64 GiB, read-only disks, and disks with a mount or an open LUKS or LVM device.
2. The user names the administrator, with a password and optional SSH keys or a GitHub user. Without a TPM2, a disk passphrase is required.
3. The user types the disk's serial number to confirm the wipe.
4. The installer makes three partitions: a 600 MiB ESP, a 2 GiB `/boot`, and LUKS2 for the rest. A random key file starts the LUKS header. The installer enrolls the TPM2 with PCR 7, a systemd recovery key, and the optional passphrase, and then removes the key file's slot.
5. `bootc install to-filesystem` runs on the live system itself, with `--source-imgref containers-storage:<embedded image>`, `--karg rd.luks.uuid=...` and `--target-imgref ghcr.io/diffsec/sibyl-os:latest-<variant>`, so `bootc upgrade` follows the registry afterwards.
6. The installer writes the administrator to `/var/lib/sibyl-install/admin.json` on the new disk. At the first boot, `sibyl-provision.service` creates the account in `wheel` and `sibyl-operators`, and deletes the file.
7. The installer prints the recovery key.

Notes on step 5:

- bootc runs under `unshare --mount`, so its mounts go away when it exits.
- It cannot run in a container of the embedded image. That image sits in a read-only image store on the squashfs, and crun cannot find `/tmp` in the nested overlay.
- bootc remounts the target read-only at the end, so the installer remounts it read-write for step 6.

### Unattended install and the VM test

`sibyl-autoinstall.service` in the live image runs the installer with no questions when the system credential `sibyl.install.json` exists, in the `sibyl-install -config` format. `tools/pxe/live-netboot.sh` network-boots the live system and passes that credential on the kernel command line. This installed the target host on 2026-10-28. The disk unlocks with the TPM2 and no passphrase, and the checks in `tools/vmtest/check-installed.sh` pass.

`tools/vmtest/install-test.sh` tests the installer in a VM with UEFI Secure Boot and a software TPM2. A systemd credential starts the installer with no questions. The test passes when all of these hold:

- the installed disk boots without a passphrase,
- the root has a TPM2 and a recovery slot and no other,
- SELinux is enforcing,
- the administrator logs in over SSH.

The CI ISO job runs the test with KVM and pushes the ISO only when it passes. That job runs only on request (`gh workflow run bootc.yml -f iso=true`), because it uses about 45 runner minutes. The target host cannot run it, because SVM is off in its firmware.

### Model pools

A model pool is an extra whole disk for models. `POST /pools` asks `sibyl-hostd` to make one:

1. The request must carry the disk's serial number. The agent refuses a disk under 16 GiB, the root disk, a disk with a mount or an open LUKS or LVM device, and a disk with active swap.
2. The disk gets one partition with LUKS2. The key file is `/etc/sibyl/pools/pool-N.key`, mode 0400, on the TPM2-unlocked root disk. A systemd recovery key is the second way in, for a move to another host. The API returns it once, and the agent never logs it.
3. XFS is mounted at `/var/lib/sibyl-pools/pool-N` through `/etc/crypttab` and `/etc/fstab`, with `nofail` and the `container_file_t` SELinux context. Root owns `/var/lib/sibyl-pools`, so the `sibyl` user cannot swap a mount point for a symlink.

The key file avoids TPM2 retries at boot. The root disk already gives the TPM2 protection, and the root holds the key.

llama-swap and the System API mount `/var/lib/sibyl-pools` at `/pools` with `rslave`. A pool that is mounted or detached later shows in the containers without a restart. The model scanner, the llama-swap generator, downloads (`"pool"` in `POST /downloads`) and the benchmark know about pools. A model ID that is in two places resolves to the models folder first, then to the lowest pool. `DELETE /pools/{name}` unloads llama-swap's models, unmounts the pool and removes it from `crypttab` and `fstab`. The data and the key file stay.

[Recovery](../guides/recovery.md) covers the disk encryption from the owner's side.

## Offline updates

A host with no internet updates from a bundle. CI builds one only for release tags or on request (`gh workflow run bootc.yml -f bundle=true`), because a bundle is about 4 GB (3.8 GB for the amd variant at sha-93a977a). It pushes the bundle to `ghcr.io/diffsec/sibyl-bundle:<tag>-<variant>`.

A bundle is a directory:

| File | Content |
|---|---|
| `manifest.json` | The tag, the variant, and each image with its reference, archive name, size and SHA-256 |
| `manifest.json.sig` | `cosign sign-blob` signature of `manifest.json` with the project key |
| `*.oci.tar` | One OCI archive per image |

`scripts/bundle/make.sh` writes the archives with `skopeo copy --preserve-digests`. The images are the host image, every Sibyl service image for the same tag (both llama-swap backends, so the backend can change offline), and the third-party images that the Quadlet units pin by digest. An OCI archive cannot hold sigstore signatures, so the signed manifest carries the trust: it lists the SHA-256 of every archive.

`sibyl-update` (`cmd/sibyl-update`, `internal/bundle`) checks the manifest's signature against `/etc/pki/containers/sibyl-cosign.pub` and the SHA-256 of every archive before it changes anything. It refuses a bundle for another GPU variant. Then:

- It loads each service image into the `sibyl` user's storage with `skopeo copy --preserve-digests`, so the digests that the Quadlet units pin still match. Each archive is first copied to `/var/lib/sibyl-update`, because the `sibyl` user cannot read the USB drive, and skopeo cannot read an archive from a pipe.
- It keeps the TUI archive in `/var/lib/sibyl-update`. The `sibyl` command loads it into the caller's own storage when the image is missing.
- It runs `bootc switch --enforce-container-sigpolicy --transport oci-archive` on the host image archive. After that, bootc follows the archive until the host switches back to the registry.

`sibyl-update verify <directory>` only checks a bundle. [Updates](../guides/updates.md) gives the procedure to apply a bundle and to return to online updates.

## Security

### Boot and host

- SELinux stays enforcing. Only Arcane runs with `SecurityLabelDisable=true`.
- Secure Boot uses the Fedora shim and kernel. The NVIDIA variant adds the project MOK key.
- nftables default-deny. Only 443, 8443, 9443 and rate-limited 22 are open.
- SSH accepts keys only. Root login is off.
- `bootc` and Podman auto-update are off by default. Updates are explicit, from the TUI, the web console, the System API, or `bootc upgrade`.
- Every Quadlet container drops all capabilities, sets `NoNewPrivileges=true` and `ReadOnly=true`, and gets a tmpfs for `/tmp`. Writable state goes to named volumes only.
- `sibyl-hostd` refuses to change or delete an account in `wheel`. Pool creation refuses a disk with active swap, as it refuses a mounted disk.

### Image signing

- CI signs every image twice (`scripts/ci/sign.sh`). The keyless signature carries the workflow identity, for anyone who checks with `cosign verify`. The key signature is what the host checks.
- The host's `/etc/containers/policy.json` requires a signature from the project key (`/etc/pki/containers/sibyl-cosign.pub`) for every image under `ghcr.io/diffsec`. podman, the Quadlet units and bootc refuse an image without it. Third-party images in the Quadlet files are pinned by digest.
- The project moved from `ghcr.io/ihavespoons` on 2026-10-28. A bridge image trusted both namespaces with the same key while hosts switched, and the old packages are deleted.
- The policy's default is `reject`, because bootc refuses a verified image reference when the default is `insecureAcceptAnything`. Common public registries are allowed as before, so Arcane can still run their images: `docker.io`, `quay.io`, `ghcr.io`, `registry.fedoraproject.org`, `gcr.io`, `registry.k8s.io`, `mcr.microsoft.com`, `public.ecr.aws`, `nvcr.io`, `registry.gitlab.com`, `cgr.dev` and `lscr.io`. Local transports such as `containers-storage` are allowed too. For any other registry, an administrator adds an entry under `docker` in `/etc/containers/policy.json`.
- podman and bootc verify only key-based sigstore signatures, and only in the older `.sig` attachment format. Keyless policies need a `subjectEmail`, which a GitHub workflow certificate does not have. CI therefore signs with `--new-bundle-format=false`. It also signs with `--recursive`, because podman checks the platform image that it picks, not only the index.
- The private key is in the `COSIGN_PRIVATE_KEY` and `COSIGN_PASSWORD` repository secrets, with an offline backup held by the maintainer. A new key needs a new public key in the image, and a host must have that image before images signed with the new key reach it.
- `/usr/lib/bootc/install/50-sibyl.toml` sets `enforce-container-sigpolicy`, so every install records a verified image reference and `bootc upgrade` checks the policy. A host installed before this change needs `bootc switch --enforce-container-sigpolicy <image>` once.
- If signing ever breaks, running containers keep running. To pull an unsigned image on purpose, an administrator changes the `ghcr.io/diffsec` entry in `/etc/containers/policy.json` on that host.
- The ISO carries the same two signatures as the images, so `cosign verify --key sibyl-cosign.pub` checks it.

### Services and accounts

- Caddy refuses Open WebUI's sign-up route on port 443. Open WebUI makes the first account the admin, so on a new install the first visitor on the LAN gets that role. The setup wizard creates the first admin through the System API instead.
- The System API replaces Arcane's default `arcane` / `arcane-admin` password with a random one when Arcane first answers. It shows the new password on the Status screen until the owner changes it in Arcane.
- Inference clients get router keys, and only operators get the System API key. The [router design](router.md#security) gives the router's own controls.

### TLS

The host has its own CA in `/var/lib/sibyl-ca` (root only). `/usr/libexec/sibyl/tls-cert` issues the server certificate from it for the current names and private IPv4 addresses, valid for 397 days.

- It runs at boot, from the NetworkManager dispatcher (`90-sibyl-tls`) on an address or hostname change, and daily (`sibyl-tls.timer`).
- It reissues only when something changed or the certificate expires within 30 days.
- The CA's name constraints limit it to local names and private ranges. The server certificate leaves out any name or address outside them.
- Clients trust `ca.crt` once.

## Delivery

| Phase | Scope | Exit check | Status |
|---|---|---|---|
| 1 | Inference images, host Containerfile, Quadlets, Caddy image, CI build and signing | The AMD host image boots in a VM. llama-swap serves a GGUF model on the 7900 XTX. | Built |
| 2 | System API and TUI in containers. Unit control from the System API. Setup wizard with GPU selection and layout. TUI editor for model settings: sampling, speculative decoding, KV cache, context and layout. Hugging Face picker. Ollama shim. Benchmark. | The API test suite passes against a running host. The wizard configures one card and two cards on the target host. | Built. The plan named the systemd D-Bus API for unit control. The built design uses the [host agent](#host-agent) and its file queue instead, so the System API container has no bus access. |
| 3 | Live ISO, persistence, guided disk installer that ends in the setup wizard, LUKS with TPM2, model pools. Privileged host changes for the wizard (network, accounts, SSH keys) as allowlisted host agent actions | Install from USB to disk on the target host, and unlock without a passphrase. | Built. The target host install passed on 2026-10-28. |
| 4 | NVIDIA and Intel variants, offline update bundles, signature policy enforced | Each variant boots. An unsigned image fails to pull. | Built. NVIDIA and Intel are not run on hardware. |
| 5 | User guide and developer guide | A new user installs Sibyl on a fresh host from the guide alone. | Built. The guides now live under `docs/getting-started/`, `docs/guides/` and `docs/development.md` (see [the docs map](../index.md)). |
| 6 | Smart model router: a gateway in front of llama-swap that serves local requests that fit and routes others to frontier providers (Anthropic, OpenAI, OpenRouter and similar), using request profiles and customisable rules, optionally decided by a small local classifier model | Exit checks per step in the [router design](router.md#delivery). | Steps 6.1 to 6.4 built, except the Open WebUI feedback signal and the `llm` classifier. |
| 7 | Web console: a browser view of the TUI, served by the System API | Exit checks per step in the [web console design](web-console.md#delivery). | Steps 7.1 to 7.4 built. |

## Open items

- The MoE offload settings have no measurements on the test host yet. A benchmark must compare `moe.cpu_layers` and `moe.cache_mib` values on a MoE model larger than the VRAM, and record the tokens per second and the VRAM in use.
- The benchmark uses one short context. Longer contexts, such as 8192 or 32768 tokens, are a later addition.
- The `capacity` router of llama-swap can replace `matrix` when a llama-swap release includes it.
- The design says that the System API detects a change of the GPU set at boot and asks the owner to run the GPU steps again. No such check is in `internal/api` yet.
- Arcane Git Sync polls and has no webhooks. Compose files pulled from Git are read-only in the Arcane UI.
- The [router](router.md#open-items) and the [web console](web-console.md#open-items) have their own open items.
