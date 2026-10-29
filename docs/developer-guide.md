# Sibyl developer guide

This guide is for people who change Sibyl: the host image, the services, the System API, the TUI, the installer and CI. It explains how the parts fit, how to build and test them, and the rules that a change must follow. [design.md](design.md) holds the design decisions and the details of each part. This guide links to it instead of repeating it.

## Contents

1. [How Sibyl fits together](#how-sibyl-fits-together)
2. [Repository layout](#repository-layout)
3. [Tools](#tools)
4. [Build and test](#build-and-test)
5. [Run the System API and the TUI locally](#run-the-system-api-and-the-tui-locally)
6. [Common changes](#common-changes)
7. [CI, images and signing](#ci-images-and-signing)
8. [Release](#release)
9. [Test on real hardware](#test-on-real-hardware)
10. [Rules](#rules)

## How Sibyl fits together

Sibyl is one bootc host image plus a set of container images. The host image is a Fedora bootc image, built from `os/Containerfile`. bootc installs and updates the whole operating system as one image, and keeps the previous version for a rollback.

```
                     LAN
                      |
           nftables: 443, 8443, 9443, 22
                      |
            +---------v----------+
            |  caddy (TLS)       |   rootless containers of the
            +--+-------+-------+-+   sibyl user (UID 1001),
               |       |       |     on the "sibyl" Podman network
     open-webui --> router    system-api      arcane
                      |   \     |   ^           |
                      v    `-> providers (HTTPS) +-- sibyl's Podman socket
                 llama-swap      |   |
                      ^          |   |
                      +----------+   |
                   config files      | files in /var/lib/sibyl/run
                                     |
        host agent (sibyl user) <----+----> sibyl-hostd (root)
        restart, reload, bench              hostname, network, accounts, pools
```

- Caddy terminates TLS and routes by port: 443 to Open WebUI, 8443 to the router (`/v1`) and the System API (`/api`, `/system`), 9443 to Arcane. nftables redirects the low ports to Caddy's high ports, so no container needs a privileged port.
- The router (`cmd/sibyl-router`) checks client keys, picks a route with the owner's rules, and forwards each request to llama-swap or to a provider. Open WebUI and the Ollama shim call it too. [router-design.md](router-design.md) describes it.
- llama-swap starts `llama-server` processes on demand from a generated configuration. One inference image runs at a time: HIP, Vulkan or CUDA.
- The System API (`cmd/sibyl-api`) owns the configuration. It writes `hardware.yaml`, `models.yaml`, the llama-swap configuration and the Quadlet drop-in that picks the inference image.
- The System API cannot reach systemd or root. It asks two agents through files:
  - The host agent (`os/rootfs/usr/libexec/sibyl/agent`) runs as the `sibyl` user. It restarts the Sibyl units, reloads systemd, and runs benchmarks. It accepts only the names in its allowlist.
  - `sibyl-hostd` (`cmd/sibyl-hostd`) runs as root. It changes the hostname, the network, operator accounts and model pools. Its checks in `internal/hostd` are the security boundary.
- The TUI (`cmd/sibyl-tui`) is a client of the System API only. `/usr/bin/sibyl` runs it in the caller's rootless Podman.
- The installer (`cmd/sibyl-install`) and the offline updater (`cmd/sibyl-update`) run on the host as root, outside every container.

[design.md](design.md) describes each part: "System API and host agent", "Inference", "Host image", "Storage", "Offline updates" and "Security".

## Repository layout

| Path | Content |
|---|---|
| `os/Containerfile` | The host image. Build argument `GPU_VARIANT` is `amd`, `nvidia` or `intel`. |
| `os/rootfs/` | Files copied to `/` of the host: Quadlet units, systemd units, nftables, policy, scripts. |
| `os/packages/` | dnf package lists, common and per variant. |
| `os/variants/` | Variant build steps. `nvidia.sh` builds and signs the NVIDIA kernel modules. |
| `containers/` | One directory per service image, and `live/` for the live USB image. |
| `cmd/` | The Go programs: `sibyl-api`, `sibyl-router`, `sibyl-tui`, `sibyl-hostd`, `sibyl-install`, `sibyl-update`. |
| `internal/` | The Go packages. Each has a package comment that says what it does. |
| `scripts/` | CI helpers, the bundle builder, and the base image bump. |
| `tools/pxe/` | Network boot and unattended install, for lab hosts. |
| `tools/vmtest/` | The VM install test that CI runs on the ISO. |
| `.github/workflows/` | `bootc.yml` builds, signs and pushes everything. `bump-base.yml` updates the base image digest. |

The Go packages, from the outside in:

| Package | Role |
|---|---|
| `api` | HTTP routes of the System API, and the apply that regenerates the configuration |
| `router` | The model router: client keys, rules, forwarding, the Anthropic Messages translation, the decision log |
| `client` | Typed client for the System API, used by the TUI |
| `tui` | The terminal interface and the setup wizard |
| `hostctl` | Requests to the host agent and to `sibyl-hostd`, and their results |
| `hostd` | The root agent: request checks, actions, model pools |
| `install` | The disk installer, persistence and first-boot provisioning |
| `bundle` | Offline bundle checks and apply |
| `llamaswap` | llama-swap configuration and Quadlet drop-in generator |
| `models` | Model scan, per-model settings and their llama-server flags |
| `hardware` | Variants, backends, `hardware.yaml`, HIP targets |
| `gpu` | GPU detection from sysfs |
| `download`, `hf` | Hugging Face client and the download manager |
| `ollama` | Ollama API shim over the router's OpenAI API |
| `owui` | Open WebUI admin API client |
| `disks` | `lsblk` parsing and disk eligibility |

## Tools

| Tool | Use |
|---|---|
| Go, at the version in `go.mod` | All Go code |
| `golangci-lint` v2 | Go lint. CI pins the version in `bootc.yml`. |
| Docker with buildx, or Podman | Image builds |
| `actionlint` | Workflow lint |
| `shellcheck` | Shell script lint |
| `hadolint` | Containerfile lint. Run it with `docker run --rm -i ghcr.io/hadolint/hadolint < <file>`. |
| `gh` | CI runs and repository settings |
| `oras` | ISO and bundle artifacts |

## Build and test

Run these before every commit. They must all pass.

```bash
go vet ./... && go test -race ./... && golangci-lint run ./...
actionlint .github/workflows/*.yml
shellcheck os/rootfs/usr/libexec/sibyl/* os/rootfs/usr/bin/sibyl scripts/*.sh scripts/*/*.sh tools/*/*.sh
```

Check the host image and its lint with a local build. The arm64 build runs on an Apple Silicon laptop. It checks the Containerfile and runs `bootc container lint`:

```bash
docker buildx build --platform linux/arm64 -f os/Containerfile -t sibyl-os:test --load .
```

The amd64 images and the HIP image build in CI only. The HIP build needs about 40 GB of disk, more than a laptop BuildKit VM has. The README lists the commands for a Linux host with enough space.

Tests that need a file layout build it in a temporary directory: a fake sysfs, a fake request queue, a fake Hugging Face server. Look at `newFixture` in `internal/api/api_test.go` and `fakeCard` in `internal/gpu/gpu_test.go` before you write a new fixture. Commands that need root, such as `cryptsetup` and `nmcli`, go through a `Runner` function, and the tests record the commands instead of running them.

## Run the System API and the TUI locally

The System API runs on a laptop with temporary directories. Every path comes from an environment variable, listed in `cmd/sibyl-api/main.go`.

```bash
d="$(mktemp -d)"
mkdir -p "$d"/{config/llama-swap,models,pools,dropin,run/requests,run/host,host-out,sys/class/drm}
echo devkey > "$d/api.key"
echo "MemTotal: 33554432 kB" > "$d/meminfo"
go build -o "$d/sibyl-api" ./cmd/sibyl-api
SIBYL_API_KEY_FILE="$d/api.key" SIBYL_CONFIG_DIR="$d/config" SIBYL_MODELS_DIR="$d/models" \
SIBYL_POOLS_DIR="$d/pools" SIBYL_DROPIN_DIR="$d/dropin" SIBYL_SYSFS="$d/sys" SIBYL_MEMINFO="$d/meminfo" \
SIBYL_RUN_DIR="$d/run" SIBYL_HOST_OUT_DIR="$d/host-out" SIBYL_LISTEN=127.0.0.1:3001 \
  "$d/sibyl-api"
```

```bash
curl -H 'Authorization: Bearer devkey' 127.0.0.1:3001/status
curl -H 'Authorization: Bearer devkey' '127.0.0.1:3001/hf/search?q=qwen3'
```

The Hugging Face routes work. The host actions return 202 and stay queued, because no `sibyl-hostd` reads the queue. Drop a `.gguf` file into `$d/models` to see a model in `GET /models` and in `$d/config/llama-swap/config.yaml`.

Run the TUI against it from a second terminal:

```bash
SIBYL_API_URL=http://127.0.0.1:3001 SIBYL_API_KEY_FILE="$d/api.key" go run ./cmd/sibyl-tui
```

Set `SIBYL_WIZARD=1` to start the setup wizard.

## Common changes

### Add a System API route

1. Add the handler in `internal/api` and register it in `Handler()` in `internal/api/api.go`. Every route except `/health` sits behind the API key.
2. Decode the body with `readJSON`. It refuses unknown fields and bodies over 1 MiB.
3. Return 422 for a value that fails a check, with an error that tells the user what to change.
4. Add the call to `internal/client`, and a test that runs the route against `newFixture`.
5. Add the route to the table in [design.md](design.md).

### Add a model setting

1. Add the field to `models.Settings`, or to `Sampling`, `Speculative` or `KVCache` in `internal/models/tuning.go`.
2. Check its range in the `Validate` method, and emit its flag in the `Flags` method.
3. If the flag is new, add it to the typed flags, so a raw argument cannot set it twice.
4. Add the field to the settings form in `internal/tui/models.go` and `internal/tui/settingsform.go`.
5. Add a row to the settings table in [design.md](design.md). A llama.cpp upgrade must recheck the flag name.

### Add a host action that needs root

This change crosses the security boundary. Read "Privileged host actions" in [design.md](design.md) first.

1. Add the `Action` constant and add it to `Actions` in `internal/hostd/request.go`.
2. Add the request body type with a `Validate` method, and decode it in `Decode`. The method must refuse every value that the action does not need. Use allowlists and strict patterns, not denylists.
3. Implement the action in `internal/hostd/agent.go` or a new file. Run fixed commands with argument lists through `a.Run`, never a shell string. Do not follow symlinks in a directory that the `sibyl` user can write.
4. Test every refusal, not only the happy path. Look at `internal/hostd/hostd_test.go`.
5. Add the API route, the client call and the TUI screen.
6. Remember that any process that runs as `sibyl` can queue the action, including Arcane and its containers. An action that can grant a login or erase data needs a stronger gate than the API key.

### Allow a new unit for restart

1. Add the unit to `hostctl.Units` in `internal/hostctl/hostctl.go`.
2. Add it to `UNITS` and `REQUEST_RE` in `os/rootfs/usr/libexec/sibyl/agent`. The two lists must match.

### Add a service container

1. Add a Quadlet `.container` file to `os/rootfs/etc/containers/systemd/users/1001/`. Drop all capabilities, set `NoNewPrivileges=true`, and use `ReadOnly=true` where the image allows it.
2. Pin a third-party image by digest. A Sibyl image uses `@IMAGE_TAG@`, which the host build replaces.
3. If the service needs a port on the host, add a Caddy site and an nftables redirect.
4. If it needs state under `/var`, add a `tmpfiles.d` line, because bootc does not update `/var` after the install.

### Change the HIP targets

`GPU_TARGETS` in `containers/llama-swap/Containerfile.hip` and `hardware.HIPTargets` must hold the same list. `TestHIPTargetsMatchImage` fails if they differ. The image build fails if the ROCm release has no rocBLAS kernels for a target. Each target adds about 15 minutes of compile time in CI.

## CI, images and signing

`bootc.yml` runs on pull requests and on pushes to `main` that touch the code or the images. A pull request builds and tests but pushes nothing.

| Job | What it does |
|---|---|
| `tag` | Picks the image tag (`sha-<7 chars>` on `main`, the version on a `v*` tag) and the variants to build. |
| `go` | `go vet`, `go test -race` and `golangci-lint`. |
| `service-images` | Builds the inference, Caddy, System API and TUI images. |
| `os-image` | Builds the host image for each variant. |
| `iso` | Builds the live ISO, installs it in a VM with Secure Boot and a TPM, and pushes it. Only on request. |
| `bundle` | Builds the offline bundle. On `v*` tags and on request. |

Each service image is tagged with a hash of its inputs, `src-<hash>`. When an image with the same hash exists, the job retags it instead of building it again. A change to `containers/llama-swap` therefore rebuilds the HIP and Vulkan images, and a change to Go code rebuilds only the System API and the TUI.

Requests that cost runner time are opt-in:

```bash
gh workflow run bootc.yml -f iso=true       # live ISO and VM test, about 45 minutes
gh workflow run bootc.yml -f bundle=true    # offline bundle, about 4 GB
gh workflow run bootc.yml -f variants=true  # nvidia and intel host images
```

### Signing

`scripts/ci/sign.sh` signs every pushed image twice. The keyless signature carries the workflow identity. The project key signature is the one that hosts check, because podman and bootc verify only key-based signatures. The host's `/etc/containers/policy.json` refuses any `ghcr.io/diffsec` image without it.

| Repository secret | Use |
|---|---|
| `COSIGN_PRIVATE_KEY`, `COSIGN_PASSWORD` | The project signing key for images and bundle manifests |
| `MOK_PRIVATE_KEY` | Signs the NVIDIA kernel modules |

A new signing key needs the new public key in `os/rootfs/etc/pki/containers/sibyl-cosign.pub`, and every host must run an image with that key before images signed only with the new key reach it.

### Image retention

`cleanup.yml` deletes old image versions every Sunday. It keeps every image from the last 14 days, the newest builds of each package beyond that (the `keep` column in the workflow), and every release and `latest` tag. It never touches `sibyl-buildcache`. A host that is further behind than that cannot pull an image that it has not pulled before, such as the other llama-swap backend.

The workflow needs the Admin role on each package, in the package's settings under "Manage Actions access". Run it by hand with the dry run first:

```bash
gh workflow run cleanup.yml -f dry_run=true
```

### Base image

Quay deletes old `fedora-bootc` manifests after the tag moves, so the digest in `os/Containerfile` goes stale. The `bump base image` workflow opens a pull request with the current digest every Monday. To do it by hand, run `scripts/bump-base-image.sh`.

## Release

1. Make sure that `main` passes CI and that the box runs the same commit.
2. Tag and push:

```bash
git tag v0.1.0
git push origin v0.1.0
```

The tag builds all three variants and the bundles, tagged `0.1.0`.

3. The ISO is not built on a tag push. Build it for the tag:

```bash
gh workflow run bootc.yml --ref v0.1.0 -f iso=true -f variants=true
```

4. Make sure that the `sibyl-iso` and `sibyl-bundle` packages are public in the GitHub package settings. New packages are private, and an anonymous `oras pull` then fails with "denied".

## Test on real hardware

### Update a test host to a build

A host follows `ghcr.io/diffsec/sibyl-os:latest-<variant>`, which every push to `main` moves. To run a specific build:

```bash
sudo bootc switch --enforce-container-sigpolicy ghcr.io/diffsec/sibyl-os:sha-<commit>-amd
sudo systemctl reboot
```

Return to the moving tag with the same command and `latest-amd`.

### Reinstall a test host over the network

`tools/pxe/live-netboot.sh` boots the live system over the network and installs with no questions. [tools/pxe/README.md](../tools/pxe/README.md) has the steps. It erases the target disk without a confirmation, so use it only on a lab host. It sends the admin password hash and SSH key over plain HTTP, so use it only on a network that you trust.

### VM install test

`tools/vmtest/install-test.sh <iso> <work dir>` installs an ISO into a QEMU VM with OVMF Secure Boot and a software TPM, boots the installed disk, and runs `tools/vmtest/check-installed.sh` over SSH. It needs KVM.

## Rules

- Fix every linter finding. Do not add exclusions or disable a check. If a finding cannot be fixed, ask the maintainer.
- Pin GitHub Actions by commit SHA, at the latest release, with the version in a comment.
- Pin third-party images by digest.
- Every container drops all capabilities and sets `NoNewPrivileges=true`. Only Arcane runs with `SecurityLabelDisable=true`.
- `bootc container lint` must pass with no warnings on the host image.
- Validate input at the System API for a clear error, and again in `sibyl-hostd`, which is the boundary.
- Write documentation in plain English: short sentences, active voice, one instruction per sentence.
- Contributions need a signed contributor license agreement. See [CONTRIBUTING.md](../CONTRIBUTING.md).
