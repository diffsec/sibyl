# AGENTS.md

Orientation for AI coding agents. Link to code and docs. Do not copy values that drift, such as versions, digests and ports.

## What this repo is

Sibyl is a Fedora bootc host image plus the container images that it runs. Read `docs/design/overview.md` first. It holds the design decisions and the phase plan. `docs/index.md` maps every other page.

## Layout

- `os/` is the bootc host image. `os/Containerfile` takes `GPU_VARIANT` (`amd`, `nvidia`, `intel`) and `IMAGE_TAG`. It builds from the repo root, filtered by `os/Containerfile.dockerignore`.
- `os/rootfs/` is copied to `/` of the host. Quadlet units are in `os/rootfs/etc/containers/systemd/users/1001/`, and they run rootless as the `sibyl` user (UID 1001).
- `containers/` holds one directory per service image. `containers/system-api` builds from the repo root.
- `cmd/` and `internal/` are the Go System API. The design is in the "System API and host agent" section of `docs/design/overview.md`, and every route is in `docs/reference/system-api.md`.
- `os/rootfs/usr/libexec/sibyl/agent` is the host agent. It is the only path from a container to systemd. Keep its allowlist in step with `hostctl.Units`.
- `cmd/sibyl-hostd` is the root agent for hostname, network and accounts. `os/Containerfile` builds it into the host image. Its checks in `internal/hostd` are the security boundary, so keep every new action in `hostd.Decode` and test the refusals. The one exception is `password-check` in `internal/hostd/auth.go`. It is kept out of `Decode` on purpose, so the disk queue refuses it, and only the memory queue `/run/sibyl-auth/req` takes it.
- `internal/modelform` is the model settings form that the TUI and the console share.
- `internal/console` is the web console at `/console/`. It calls the System API through `internal/client` in process. Keep every console change on an existing API route, and `docs/design/web-console.md` in step.
- `.github/workflows/bootc.yml` builds, pushes and signs all images.

## Rules

- Pin third-party images by digest, and pin GitHub Actions by commit SHA at the latest release.
- Quay deletes old `fedora-bootc` manifests after the tag moves. If the host build fails with `not found` on the base image, run `scripts/bump-base-image.sh`. The `bump base image` workflow does this weekly.
- Fix linter findings. Do not add exclusions. Ask the maintainer if a finding cannot be fixed.
- `bootc container lint` must pass with no warnings.
- Every container drops all capabilities and sets `NoNewPrivileges=true`. Only Arcane runs with `SecurityLabelDisable=true`, because its Podman socket and D-Bus mounts need it.
- State under `/var` needs a `tmpfiles.d` entry, because bootc does not update `/var` after install.
- Users and groups come from `sysusers.d`. The `render` and `video` groups are copied into `/etc/group` at build time, so that sysusers can add members.

## Verify

```bash
go vet ./... && go test -race ./... && golangci-lint run ./...
docker buildx build --platform linux/arm64 -f os/Containerfile -t sibyl-os:test --load .
actionlint .github/workflows/bootc.yml
```

The arm64 build checks the Containerfile and runs `bootc container lint`. The amd64 images build in CI only, because the ROCm build needs more memory and disk than a laptop BuildKit VM has.
