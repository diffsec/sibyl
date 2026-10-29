# Sibyl design

Sibyl is an all-in-one inference server: a hardened, immutable Linux host that boots from USB or installs to disk, and serves OpenAI- and Ollama-compatible APIs from llama.cpp, with a chat UI, a model manager and a container platform. Each GPU vendor has its own host image variant. The AMD variant is tested on a host with two Radeon RX 7900 XTX cards. The NVIDIA and Intel variants build and pass the same checks, but no one has run them on hardware yet.

This document records the design decisions from October 2026 and the delivery phases.

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
| Disks | Guided installer, LUKS with TPM2 unlock, model storage pools, USB persistence | See "Storage". |
| Hardening | SELinux enforcing, Secure Boot, signed images, CIS-style lockdown | See "Security". |
| Updates | GHCR registry, plus offline bundles | `bootc upgrade` online. `bootc switch` to an OCI archive offline. |
| CI | GitHub Actions, SHA-pinned actions, cosign keyless and key signing | Images and ISOs build in CI. The host verifies the key signature. |
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
| `llama-swap` | `ghcr.io/diffsec/sibyl-llama-swap:<ver>-hip` or `-vulkan` | 8080 | No |
| `router` | `ghcr.io/diffsec/sibyl-router` | 8080 | No |
| `open-webui` | `ghcr.io/open-webui/open-webui` (pinned digest) | 8080 | No |
| `system-api` | `ghcr.io/diffsec/sibyl-system-api` | 3001 | No |
| `arcane` | `ghcr.io/getarcaneapp/manager` (pinned digest) | 3552 | No |
| `caddy` | `ghcr.io/diffsec/sibyl-caddy` | 10443, 18443, 19443 | Yes, through nftables redirect |

nftables redirects host port 443 to 10443 (Open WebUI), 8443 to 18443 (the router's OpenAI and Anthropic API, the Ollama shim, and the System API), and 9443 to 19443 (Arcane).

Caddy sends `/v1/*` to the model router, which checks each client's key and policy and then forwards the request to llama-swap or to a provider. Open WebUI and the Ollama shim also call the router. [router-design.md](router-design.md) describes the router. The input chain accepts the high ports only for redirected connections. This keeps `net.ipv4.ip_unprivileged_port_start` at its default of 1024.

## System API and host agent

The System API is a Go service (`cmd/sibyl-api`, `internal/`). It runs in a container with no access to the systemd bus, so SELinux confinement stays on. It reads and writes only these directories:

| Host path | In the container | Content |
|---|---|---|
| `/var/lib/sibyl/config` | `/config` | `hardware.yaml`, `models.yaml`, `llama-swap/config.yaml` |
| `/var/lib/sibyl/models` | `/models` | GGUF files |
| `/var/lib/sibyl/run` | `/run/sibyl` | Request queue and `status.json` |
| `/var/lib/sibyl/home/.config/containers/systemd/llama-swap.container.d` | `/quadlet` | `backend.conf`, which overrides the image default |

Each change to the hardware or to model settings runs an apply. The apply regenerates the llama-swap configuration, and llama-swap reloads it without a restart. llama-swap polls the file with `stat`, so the configuration is mounted as a directory: a single-file bind mount keeps the old inode after an atomic rename. If the backend or the GPU set changes, the apply also rewrites `backend.conf` and queues a reload and a restart.

The host agent (`/usr/libexec/sibyl/agent`) does the unit control. The System API creates an empty file named `<timestamp>.<action>.<unit>` in the request queue. The `sibyl-agent.path` user unit starts the agent, which accepts only `restart` and `reload` for the six Sibyl units and deletes every request. The `sibyl-status.timer` writes the unit states to `status.json` every 15 seconds.

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
| `GET, PUT, DELETE /integrations/huggingface` | Hugging Face sign-in: the account, set a token, or sign out |
| `GET /hf/search?q=` | Search Hugging Face for GGUF repositories, most downloaded first |
| `GET /hf/repos/{owner}/{name}` | Complete GGUF variants at a pinned commit, with size and fit (`gpu`, `split` or `no`) |
| `GET, POST /downloads` | List download jobs, or queue one: `{"repo", "variant", "revision"}` |
| `GET, DELETE /downloads/{id}`, `POST /downloads/{id}/retry` | One job: progress, cancel, or resume a failed job |
| `GET /host` | Hostname, network interfaces, accounts, and a network change that waits for a confirm |
| `PUT /host/hostname` | Set the static hostname |
| `PUT /host/network/{iface}`, `POST /host/network/confirm` | DHCP or a static IPv4 address, with automatic rollback |
| `PUT, DELETE /host/users/{name}` | Create an operator account or replace its SSH keys, or delete it |
| `GET /host/requests/{id}` | The result of a host request that took longer than 45 seconds |
| `GET, POST /pools`, `DELETE /pools/{name}` | Model pools and candidate disks, make a pool, or detach one |
| `GET /router` | Router clients, providers and profiles |
| `POST /router/clients`, `PUT, DELETE /router/clients/{name}` | Create a client and show its key once, change its policy and budget, or delete it |
| `POST /router/clients/{name}/key` | Give a client a new key, shown once. The old key stops working. |
| `PUT, DELETE /router/providers/{name}` | Add or change a provider after a check of its key, or delete it |
| `PUT /router/profiles` | Replace the routing profiles after the router's checks |
| `POST /router/explain` | The route for a request, with every rule, without sending it |
| `GET /router/stats` | Requests per client and per route, spend per client, and the last decisions |

Extra llama-server arguments must match `^[A-Za-z0-9_.,:=/+-]+$`. Flags that Sibyl sets, such as `--model`, `--port` and `--device`, are rejected.

### Privileged host actions

The setup wizard changes the hostname, the network and the login accounts. These changes need root, and the System API runs as `sibyl`. A second agent, `sibyl-hostd` (`cmd/sibyl-hostd`, `internal/hostd`), runs as root and does them.

1. The System API writes one JSON request per file into `/var/lib/sibyl/run/host`. It checks the request with the same rules as `sibyl-hostd` first, and returns 422 for a bad request.
2. `sibyl-hostd.path` starts `sibyl-hostd.service`. The agent reads each request without following symlinks, deletes it, and checks it again. Only the agent's checks protect the host.
3. The agent runs fixed commands with arguments, never a shell string: `hostnamectl`, `nmcli`, `busctl`, `useradd`, `usermod`, `userdel`, `runuser` and `restorecon`.
4. It writes a result to `/var/lib/sibyl-host/out/results/<id>.json` and the host state to `/var/lib/sibyl-host/out/state.json`. The System API mounts that directory read-only, and waits up to 45 seconds for the result.

The rules keep root out of reach of the System API. Any process that runs as `sibyl` can queue requests, and that includes Arcane and the containers it starts.

- Accounts go into `sibyl-operators` only, never into `wheel`. An operator can run `sibyl`, but not `sudo`.
- The agent changes and deletes only the accounts that it created. It records them in `/var/lib/sibyl-host/managed-users.json`, which only root can read. `root`, `admin`, `sibyl` and other system names are refused.
- SSH keys must be `ssh-ed25519`, ECDSA, security-key types, or RSA of 2048 bits or more. Key options such as `command=` are refused. The account itself writes its `authorized_keys` through `runuser`, so a symlink in its home cannot redirect a write by root.
- `PUT /host/users/{name}` can also take keys from `https://github.com/<user>.keys`.
- Deleting an operator also deletes its home directory.

A network change runs inside a NetworkManager checkpoint. If no confirm arrives in 120 seconds (30 to 600 can be set), NetworkManager restores the old configuration by itself. A wrong address therefore cannot lock out a remote user. The user reconnects at the new address and calls `POST /host/network/confirm`, or presses `c` in the TUI. Only IPv4 on ethernet interfaces is supported. The NetworkManager dispatcher then starts `sibyl-tls.service`, which issues a new server certificate for the new address (see "TLS").

`sibyl-hostd-state.timer` rewrites the state every 5 minutes, so accounts added by hand also show.

### Console interface

The TUI (`cmd/sibyl-tui`, `internal/tui`) is a container image, `sibyl-tui`. It talks only to the System API, so it has no host privileges. Members of the `sibyl-operators` group run it with `sibyl`, with no sudo. The command starts the TUI in the caller's own rootless Podman, read-only with all capabilities dropped. The TUI connects through Caddy on `127.0.0.1:18443` and verifies the host certificate as `sibyl.local`.

At each boot, `sibyl-operator-export.service` copies the System API key, the `default` inference key and the CA certificate to `/var/lib/sibyl-operator/`, readable by `sibyl-operators` only. Membership gives full control of the System API, so add only trusted users: `sudo usermod -aG sibyl-operators <user>`.

| Screen | Purpose |
|---|---|
| Status | GPUs with VRAM in use, and service states with restarts |
| Hardware | The GPU part of the setup wizard: GPUs for inference, backend, default layout |
| Models | Installed models, the settings editor, and delete |
| Download | Hugging Face search, variants with fit, confirmation, and download jobs |
| Host | Hostname, network with rollback, model pools, and operator accounts with SSH keys |

### Model downloads

The download manager (`internal/download`) runs one job at a time. Each job pins a commit, so every file comes from one revision even if the repository changes during the download.

1. The job checks that the disk has room for the rest of the download plus 2 GiB.
2. Files go to `/models/.partial-<job>/`. The model scanner skips hidden folders, so a half-downloaded model is never listed.
3. A dropped transfer resumes with an HTTP Range request. An attempt that receives no data for 2 minutes is retried, up to 10 times.
4. Each file must match the SHA-256 that Hugging Face records for it. A file that fails is deleted.
5. The folder is renamed to `/models/<variant>/`, with a `.sibyl-source.json` that records the repository, commit and digests. The API then regenerates the llama-swap configuration.

Jobs are saved in `/config/downloads.json` and resume after a restart.

Gated and private repositories need a Hugging Face access token. In the TUI, press `t` on the Download screen. The route is `PUT /integrations/huggingface` with `{"token": "hf_..."}`. The System API checks the token with Hugging Face's `/api/whoami-v2` and stores it in `/config/hf-token` with mode 0600, and the account name in `/config/huggingface.json`. Searches, repository listings and downloads use it at once, with no restart. `DELETE /integrations/huggingface` signs out. A gated model also needs its terms accepted on huggingface.co with the same account. When Hugging Face refuses a request, the error says so, and a download fails at once instead of retrying, so Retry (`R`) resumes it after the fix.

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

### Context size and memory estimate

`GET /models` includes each model's GGUF metadata (`internal/gguf`, `models.ReadMeta`): the architecture, the native context, the per-token KV cache elements, and the sampling defaults in `general.sampling.*`. The System API caches it by file size and modification time. The reader reads only the header, and refuses lengths over fixed limits.

With no `ctx_size`, llama-server uses the native context, which is often 262,144 tokens. The TUI's context field shows the native context and an estimate of the GPU memory for the chosen context and KV cache types, and says when the total passes 92% of the VRAM of the GPUs that the model's layout uses. llama.cpp then keeps the rest in system RAM: on the test host, a 63 GB model at a 131,072 token context ran with 42 GB on two 24 GB GPUs. The estimate per token is:

- the K and V elements of every layer that keeps a full-length cache, at the cache type's size: recurrent layers, sliding-window layers and multi-token prediction layers are left out;
- the sparse attention indexer, if the model has one, in f16;
- 8 KiB of compute buffer;
- for a quantized cache, 4 bytes for each K and V element of the largest layer.

These terms come from measurements on the test host (llama.cpp b11889, ROCm): the VRAM change between a 32,768 and a 131,072 token context. The estimate matched six of seven measurements within 0.4%, across a dense model, a hybrid model, and a sliding-window model at f16, q8_0 and q4_0. It was 3% low on a model with compressed attention layers. `internal/models/meta_test.go` holds the measured values.

The sampling fields show the model file's recommendation, or llama.cpp's default, as their placeholder. llama-server applies the file's values when a setting is empty: `/props` on the test host reported the file's temperature 1.0 and top-k 20.

## Inference

### Images

`containers/llama-swap/Containerfile.hip` builds llama.cpp on `rocm/dev-ubuntu-24.04:<ROCm>-complete` for the Radeon and Ryzen targets that llama.cpp's own ROCm image builds, without the Instinct cards. The runtime stage keeps only the HIP runtime libraries that `llama-server` links against, and the rocBLAS and hipBLASLt kernel files for those targets. The build fails if ROCm has no rocBLAS kernels for a target, because rocBLAS would otherwise fail at run time on that card. One target took about 15 minutes to compile on a GitHub runner.

`hardware.HIPTargets` holds the same list, and a test keeps it in step with `GPU_TARGETS`. `gpu.Detect` reads each AMD card's target from the amdkfd topology in sysfs (`gfx_target_version`, matched to the card by its render node). `PUT /hardware` refuses HIP for a card outside the list, and with no `hardware.yaml` the default backend is Vulkan when GPU 0 is outside it. The benchmark runs only the backends that every selected card supports. A card whose target is unknown counts as supported.

`containers/llama-swap/Containerfile.vulkan` builds llama.cpp with `GGML_VULKAN=ON` on Ubuntu 26.04. Mesa is the Vulkan driver for AMD (RADV) and Intel (ANV). The image has no ROCm userspace. It uses Ubuntu, not Wolfi, to match the upstream llama.cpp Vulkan build.

`containers/llama-swap-cuda/Containerfile` starts from llama.cpp's own CUDA image (`ghcr.io/ggml-org/llama.cpp:full-cuda-<release>`, pinned by digest), so CI compiles no CUDA. It moves llama.cpp to `/opt/llama`, like the other images. The NVIDIA driver and `libcuda` come from the host through CDI, so the library check allows only `libcuda.so.1` to be missing at build time.

Both images add the llama-swap release binary and run as UID 10001 inside the container.

Build arguments:

| Argument | Default | Purpose |
|---|---|---|
| `LLAMA_CPP_REF` | pinned `bNNNNN` tag | llama.cpp release |
| `LLAMA_SWAP_VERSION` | pinned release | llama-swap release |
| `ROCM_VERSION` | `7.2.4` | ROCm base. Upstream llama.cpp tests 7.2.x. ROCm 7.14 is a benchmark candidate. |
| `GPU_TARGETS` | `gfx1030;gfx1100;gfx1101;gfx1102;gfx1150;gfx1151;gfx1200;gfx1201` | RDNA2 (RX 6800 and 6900, PRO W6800), RDNA3 (RX 7000, PRO W7000), RDNA3.5 (Ryzen AI 300, Strix Halo), RDNA4 (RX 9000, AI PRO R9700) |
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

Device names differ per backend: `ROCm0` for HIP, `CUDA0` for CUDA and `Vulkan0` for Vulkan. The System API writes the llama-swap configuration, so it fills in the right names. It also writes the backend drop-in (`internal/llamaswap` `DropIn`), which picks the image, the GPU devices (`/dev/kfd` and `/dev/dri` for HIP, `/dev/dri` for Vulkan on AMD and Intel, and the CDI device `nvidia.com/gpu=all` on NVIDIA) and the variable that limits the visible GPUs. The System API learns the variant from `SIBYL_GPU_VARIANT`, and accepts only that variant's backends.

The multi-GPU table above uses HIP device names, from the AMD test host. The other backends work the same way.

### Benchmark

`POST /models/{id}/benchmark` (key `b` in the TUI) compares the backends on one model. The host agent unloads llama-swap's models, then runs `llama-bench` in the image of each backend that the variant ships and that every selected GPU supports, on the selected GPUs: 512 prompt tokens and 128 generated tokens, twice each. `GET /models/{id}/benchmark` and the model list report tokens per second for each backend, and recommend the faster one by generation speed.

llama-swap runs one image, so the backend applies to every model. The recommendation is advice: the user sets the backend in the Hardware screen. Longer contexts, such as 8192 or 32768 tokens, are a later addition.

### Embeddings for Open WebUI

Open WebUI's document search uses an embedding model from llama-swap through its OpenAI engine. The matrix router keeps the embedding model loaded beside a chat model.

Open WebUI stores its embedding settings in its database (`rag.embedding_model` is persistent configuration in v0.12.4), so environment variables apply on the first start only. Sibyl sets them through Open WebUI's admin API instead:

1. In the TUI (Status screen, key `o`), the admin either signs in with an existing Open WebUI admin account, or creates the first admin on a fresh install, or pastes an API key. The API routes are `POST /integrations/open-webui/connect` and `PUT /integrations/open-webui`.
2. With a login, the System API signs in (or signs up when no account exists, which Open WebUI makes the admin), turns on API keys in the admin config if they are off, and reads the admin's API key. It creates a key only when the account has none, because creating one replaces the old key. The password is used for that request only and is never stored.
3. The System API checks the key against an admin-only endpoint, then stores it in `/config/open-webui.key` with mode 0600.
4. After each configuration change, and at startup, the System API points Open WebUI at the first installed model marked as an embedding model, through `POST /api/v1/retrieval/embedding/update`. It changes nothing when Open WebUI already uses that model.

### Ollama-compatible API

Many clients speak only the Ollama API, and llama-swap has none. The System API serves `/api/version`, `/api/tags`, `/api/show`, `/api/ps`, `/api/chat`, `/api/generate`, `/api/embed` and `/api/embeddings` (`internal/ollama`). Each call translates to llama-swap's OpenAI API. Caddy routes `/api/*` on port 8443 to the System API, and `/v1/*` to llama-swap. Both need the API key.

- Streaming turns OpenAI server-sent events into Ollama's NDJSON frames, with `thinking` from llama-server's `reasoning_content`.
- Tool calls convert in both directions: Ollama sends arguments as a JSON object, and OpenAI as a JSON string.
- Ollama `options` map to OpenAI and llama-server fields: `temperature`, `top_p`, `top_k`, `min_p`, `repeat_penalty`, `presence_penalty`, `seed`, `num_predict`, `stop`. `format` maps to JSON mode or a JSON schema.
- `/api/pull`, `/api/create` and the other model management calls return 501, because Sibyl downloads models through its own picker.

## Host image

`os/Containerfile` starts from `quay.io/fedora/fedora-bootc:44` and adds:

- The packages in `os/packages/common.txt` and `os/packages/<variant>.txt`.
- The `sibyl` user, its subordinate ID ranges, and lingering so its user manager starts at boot.
- Quadlet units, the Podman network, sysctl and nftables rules.
- The container signature policy in `/etc/containers/policy.json`.
- Host-only tools that cannot run in a container: GPU detection, the installer and disk management.

The AMD variant needs no extra kernel modules. `amdgpu` is in the Fedora kernel and is signed for Secure Boot. The host needs only the firmware (`amd-gpu-firmware`) and device access. The ROCm userspace is in the HIP image only.

The Intel variant also uses in-kernel drivers (`i915` and `xe`) and only adds the firmware (`intel-gpu-firmware`). Its inference image is the Vulkan one, with Mesa ANV.

The NVIDIA variant runs `os/variants/nvidia.sh` during the image build:

1. It adds RPM Fusion and the NVIDIA Container Toolkit repository.
2. It builds the driver's open kernel modules with `akmods` for the image's kernel. NVIDIA recommends the open modules for Turing and newer GPUs.
3. akmods signs the modules with the project MOK key, which CI passes as the build secret `mok_key` (the `MOK_PRIVATE_KEY` repository secret). The key exists only inside that one build step, so no image layer holds it. The public certificate is in the image at `/usr/share/sibyl/sibyl-mok.der`.
4. It enables `nvidia-cdi-refresh`, which writes the CDI devices at boot, and sets kernel arguments in `/usr/lib/bootc/kargs.d` that keep `nouveau` off the GPU.

Secure Boot loads the modules only after the host enrolls the MOK certificate once: `sudo mokutil --import /usr/share/sibyl/sibyl-mok.der`, then a reboot and the confirmation in the MOK manager. The model fit estimate shows `unknown` on NVIDIA and Intel, because their drivers do not report VRAM in sysfs the way `amdgpu` does.

CI builds the NVIDIA and Intel host images only for release tags or with `gh workflow run bootc.yml -f variants=true`, to save runner minutes. The CUDA inference image builds with the others.

The NVIDIA variant adds `akmod-nvidia` built at image build time and signed with a project MOK key. The user enrolls that key once with `mokutil`. The Intel variant adds `intel-compute-runtime` to the host for device nodes, and the inference image uses Vulkan or SYCL.

### GPU access from rootless containers

The `sibyl` user is in the `render` and `video` groups. Quadlet passes `/dev/kfd` and `/dev/dri` with `AddDevice=` and keeps the groups with `GroupAdd=keep-groups`. SELinux blocks device access by `container_t` unless the `container_use_devices` boolean is on. `sibyl-selinux.service` sets it at each boot.

## Storage

| Mode | Root | Models and data | Unlock |
|---|---|---|---|
| Live USB | Read-only squashfs on the USB, with a RAM overlay | Persistence partition on the USB, XFS label `SIBYLPERSIST`, mounted at `/var/lib/sibyl` | None |
| Installed | bootc on internal disk, A/B deployments | `/var` on the root disk, plus pools under `/var/lib/sibyl-pools/pool-N` | TPM2 with PCR 7, and a recovery key |

### Live USB

`containers/live/Containerfile` adds a live boot to the host image: `dmsquash-live` in the initramfs, shim and GRUB for an ISO, and `iso.yaml`. CI turns that image into an ISO with image-builder's `generic-iso` type. The ISO also holds the host image in its container storage (`--bootc-installer-payload-ref`), so the installer works offline. CI pushes the ISO to `ghcr.io/diffsec/sibyl-iso:<tag>-<variant>` as a signed OCI artifact, because it is larger than the 2 GiB limit for release assets.

- Download the ISO with `oras pull ghcr.io/diffsec/sibyl-iso:latest-amd`.
- image-builder boots the live system with `enforcing=0`, because the squashfs has no SELinux labels. The installed system runs enforcing.
- The first console logs in as root and shows a menu: install, make a persistence partition, shell, reboot.
- Without persistence, the Sibyl services do not start, because their images do not fit in RAM. `sibyl-install persist` adds an XFS partition in the free space after the ISO on the USB. At the next boot it mounts at `/var/lib/sibyl`, and the services start from it.

### Guided installer

`sibyl-install` (`cmd/sibyl-install`, `internal/install`) runs as root from the live menu. bootc's own `--block-setup tpm2-luks` keeps no recovery key, so the installer sets up LUKS itself and then runs `bootc install to-filesystem`, as the bootc documentation recommends.

1. The user picks a disk. The installer lists disks with `lsblk -J`, and refuses the boot USB, disks under 64 GiB, read-only disks, and disks with a mount or an open LUKS or LVM device.
2. The user names the administrator, with a password and optional SSH keys or a GitHub user. Without a TPM2, a disk passphrase is required.
3. The user types the disk's serial number to confirm the wipe.
4. The installer makes three partitions: a 600 MiB ESP, a 2 GiB `/boot`, and LUKS2 for the rest. A random key file starts the LUKS header. The installer enrolls the TPM2 with PCR 7, a systemd recovery key, and the optional passphrase, and then removes the key file's slot.
5. `bootc install to-filesystem` runs on the live system itself, with `--source-imgref containers-storage:<embedded image>`, `--karg rd.luks.uuid=...` and `--target-imgref ghcr.io/diffsec/sibyl-os:latest-<variant>`, so `bootc upgrade` follows the registry afterwards. It runs under `unshare --mount`, so its mounts go away when it exits. It cannot run in a container of the embedded image: that image sits in a read-only image store on the squashfs, and crun cannot find `/tmp` in the nested overlay. bootc remounts the target read-only at the end, so the installer remounts it read-write for the next step.
6. The installer writes the administrator to `/var/lib/sibyl-install/admin.json` on the new disk. At the first boot `sibyl-provision.service` creates the account in `wheel` and `sibyl-operators`, and deletes the file.
7. The installer prints the recovery key.

`sibyl-autoinstall.service` in the live image runs the installer with no questions when the system credential `sibyl.install.json` exists, in the `sibyl-install -config` format. `tools/pxe/live-netboot.sh` network-boots the live system and passes that credential on the kernel command line. This installed the target host on 2026-10-28: the disk unlocks with the TPM2 and no passphrase, and the checks in `tools/vmtest/check-installed.sh` pass.

`tools/vmtest/install-test.sh` tests all of this in a VM with UEFI Secure Boot and a software TPM2. A systemd credential starts the installer with no questions. The test passes when the installed disk boots without a passphrase, the root has a TPM2 and a recovery slot and no other, SELinux is enforcing, and the administrator logs in over SSH. The CI ISO job runs it with KVM and pushes the ISO only when it passes. That job runs only on request (`gh workflow run bootc.yml -f iso=true`), because it uses about 45 runner minutes. The target host cannot run it, because SVM is off in its firmware.

### Model pools

A model pool is an extra whole disk for models. `POST /pools` (key `p` on the Host screen) asks `sibyl-hostd` to make one:

1. The request must carry the disk's serial number. The agent refuses a disk under 16 GiB, the root disk, and a disk with a mount or an open LUKS or LVM device.
2. The disk gets one partition with LUKS2. The key file is `/etc/sibyl/pools/pool-N.key`, mode 0400, on the TPM2-unlocked root disk. A systemd recovery key is the second way in, for a move to another host. The API returns it once, and the agent never logs it.
3. XFS is mounted at `/var/lib/sibyl-pools/pool-N` through `/etc/crypttab` and `/etc/fstab`, with `nofail` and the `container_file_t` SELinux context. Root owns `/var/lib/sibyl-pools`, so the `sibyl` user cannot swap a mount point for a symlink.

The key file avoids TPM2 retries at boot. The root disk already gives the TPM2 protection, and the root holds the key.

llama-swap and the System API mount `/var/lib/sibyl-pools` at `/pools` with `rslave`. A pool that is mounted or detached later shows in the containers without a restart. The model scanner, the llama-swap generator, downloads (`"pool"` in `POST /downloads`) and the benchmark know about pools. A model ID that is in two places resolves to the models folder first, then to the lowest pool. `DELETE /pools/{name}` unloads llama-swap's models, unmounts the pool and removes it from `crypttab` and `fstab`. The data and the key file stay.

## Setup wizard

The setup wizard configures the hardware and the services. It runs in the TUI in three places: at the end of the disk installer, at the first boot of a live USB, and on demand with `sibyl setup`. Every step can run again later without a reinstall.

1. Detect the GPUs. The System API reads sysfs and lists each card with its vendor, ROCm target on AMD, VRAM, and PCIe link width.
2. Choose the GPUs for inference. The user can exclude a card, for example a card that drives a display.
3. Choose the default layout for the selected GPUs: span one model across all of them, or pin one model per card. A model can override the default later.
4. Choose the backend: HIP, Vulkan, or "benchmark". The benchmark runs a short `llama-bench` on both images with a small model and keeps the faster one.
5. Set the network: hostname, DHCP or a static address.
6. Set the Open WebUI admin account and import SSH public keys.
7. Optionally download a first model from the curated list, sized to the VRAM of the selected GPUs.
8. Make an API key for each program on the Router screen.
9. Show where the keys are, and the fingerprint of the Sibyl CA certificate.

`sibyl setup` starts the TUI in wizard mode (`internal/tui/wizard.go`). Each step opens the normal screen for it and says which key to press, and `]` and `[` move between steps. The last step shows the client URLs, where the keys are, and the SHA-256 fingerprint of the Sibyl CA certificate. The installer's last message and the live menu both point to `sibyl setup`. The benchmark choice in step 4 is the `b` key on the Models screen, and it needs an installed model.

The wizard writes `/var/lib/sibyl/config/hardware.yaml`. The System API reads that file and generates three outputs:

- The llama-swap configuration, with device names and matrix sets for the chosen layout.
- The backend drop-in `llama-swap.container.d/backend.conf`, with the image and the GPU visibility variable (`HIP_VISIBLE_DEVICES` for HIP, `GGML_VK_VISIBLE_DEVICES` for Vulkan).
- The model size limits that the model picker uses.

When the GPU set changes, for example after a card is added, the System API detects the difference at boot and asks the user to run the GPU steps again.

## Offline updates

A host with no internet updates from a bundle. CI builds one only for release tags or on request (`gh workflow run bootc.yml -f bundle=true`), because a bundle is about 4 GB (3.8 GB for the amd variant at sha-93a977a). It pushes the bundle to `ghcr.io/diffsec/sibyl-bundle:<tag>-<variant>`.

A bundle is a directory:

| File | Content |
|---|---|
| `manifest.json` | The tag, the variant, and each image with its reference, archive name, size and SHA-256 |
| `manifest.json.sig` | `cosign sign-blob` signature of `manifest.json` with the project key |
| `*.oci.tar` | One OCI archive per image |

`scripts/bundle/make.sh` writes the archives with `skopeo copy --preserve-digests`. The images are the host image, every Sibyl service image for the same tag (both llama-swap backends, so the backend can change offline), and the third-party images that the Quadlet units pin by digest. An OCI archive cannot hold sigstore signatures, so the signed manifest carries the trust: it lists the SHA-256 of every archive.

To update a host:

1. On a machine with internet, run `oras pull ghcr.io/diffsec/sibyl-bundle:<tag>-<variant>` into a directory on a USB drive.
2. Mount the drive on the host, and run `sudo sibyl-update apply <directory>`.
3. Reboot with `sudo systemctl reboot`.

`sibyl-update` (`cmd/sibyl-update`, `internal/bundle`) checks the manifest's signature against `/etc/pki/containers/sibyl-cosign.pub` and the SHA-256 of every archive before it changes anything. It refuses a bundle for another GPU variant. Then:

- It loads each service image into the `sibyl` user's storage with `skopeo copy --preserve-digests`, so the digests that the Quadlet units pin still match. Each archive is first copied to `/var/lib/sibyl-update`, because the `sibyl` user cannot read the USB drive, and skopeo cannot read an archive from a pipe.
- It keeps the TUI archive in `/var/lib/sibyl-update`. The `sibyl` command loads it into the caller's own storage when the image is missing.
- It runs `bootc switch --enforce-container-sigpolicy --transport oci-archive` on the host image archive. After that, bootc follows the archive, so the host returns to online updates with `sudo bootc switch --enforce-container-sigpolicy ghcr.io/diffsec/sibyl-os:latest-<variant>`.

`sibyl-update verify <directory>` only checks a bundle.

## Security

- SELinux stays enforcing. Only Arcane runs with `SecurityLabelDisable=true`.
- Secure Boot uses the Fedora shim and kernel. The NVIDIA variant adds the project MOK key.
- CI signs every image twice (`scripts/ci/sign.sh`). The keyless signature carries the workflow identity, for anyone who checks with `cosign verify`. The key signature is what the host checks.
- The host's `/etc/containers/policy.json` requires a signature from the project key (`/etc/pki/containers/sibyl-cosign.pub`) for every image under `ghcr.io/diffsec`. podman, the Quadlet units and bootc refuse an image without it. The project moved from `ghcr.io/ihavespoons` on 2026-10-28. A bridge image trusted both namespaces with the same key while hosts switched, and the old packages are deleted. Third-party images in the Quadlet files are pinned by digest.
- The policy's default is `reject`, because bootc refuses a verified image reference when the default is `insecureAcceptAnything`. Common public registries are allowed as before, so Arcane can still run their images: `docker.io`, `quay.io`, `ghcr.io`, `registry.fedoraproject.org`, `gcr.io`, `registry.k8s.io`, `mcr.microsoft.com`, `public.ecr.aws`, `nvcr.io`, `registry.gitlab.com`, `cgr.dev` and `lscr.io`. Local transports such as `containers-storage` are allowed too. For any other registry, an administrator adds an entry under `docker` in `/etc/containers/policy.json`.
- podman and bootc verify only key-based sigstore signatures, and only in the older `.sig` attachment format. Keyless policies need a `subjectEmail`, which a GitHub workflow certificate does not have. So CI signs with `--new-bundle-format=false`, and with `--recursive`, because podman checks the platform image that it picks, not only the index.
- The private key is in the `COSIGN_PRIVATE_KEY` and `COSIGN_PASSWORD` repository secrets, with an offline backup held by the maintainer. A new key needs a new public key in the image, and a host must have that image before images signed with the new key reach it.
- `/usr/lib/bootc/install/50-sibyl.toml` sets `enforce-container-sigpolicy`, so every install records a verified image reference and `bootc upgrade` checks the policy. A host installed before this change needs `bootc switch --enforce-container-sigpolicy <image>` once.
- If signing ever breaks, running containers keep running. To pull an unsigned image on purpose, an administrator changes the `ghcr.io/diffsec` entry in `/etc/containers/policy.json` on that host.
- The ISO carries the same two signatures as the images, so `cosign verify --key sibyl-cosign.pub` checks it.
- Caddy refuses Open WebUI's sign-up route on port 443. Open WebUI makes the first account the admin, so on a new install the first visitor on the LAN would get that role. The setup wizard creates the first admin through the System API instead.
- The System API replaces Arcane's default `arcane` / `arcane-admin` password with a random one when Arcane first answers, and shows it on the Status screen until the owner changes it in Arcane.
- `sibyl-hostd` refuses to change or delete an account in `wheel`, even one on its managed list. Pool creation refuses a disk with active swap, as it refuses a mounted disk.
- TLS: the host has its own CA in `/var/lib/sibyl-ca` (root only). `/usr/libexec/sibyl/tls-cert` issues the server certificate from it for the current names and private IPv4 addresses, valid for 397 days. It runs at boot, from the NetworkManager dispatcher (`90-sibyl-tls`) on an address or hostname change, and daily (`sibyl-tls.timer`), and reissues only when something changed or the certificate expires within 30 days. The CA's name constraints limit it to local names and private ranges, and the server certificate leaves out any name or address outside them. Clients trust `ca.crt` once.
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
| 5 | User guide (`docs/user-guide.md`) and developer guide (`docs/developer-guide.md`) | A new user installs Sibyl on a fresh host from the guide alone. |
| 6 | Smart model router: a gateway in front of llama-swap that serves local requests that fit and routes others to frontier providers (Anthropic, OpenAI, OpenRouter and similar), using request profiles and customisable rules, optionally decided by a small local classifier model | Design in `docs/router-design.md`. Exit checks per step there. |

## Open items

- Arcane Git Sync polls and has no webhooks. Compose files pulled from Git are read-only in the Arcane UI.
