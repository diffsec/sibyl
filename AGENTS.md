# AGENTS.md

Orientation for AI coding agents. Link to code and docs. Do not copy values that drift, such as versions, digests and ports.

## What this repo is

Sibyl is a Fedora bootc host image plus the container images that it runs. Read `docs/design.md` first. It holds every design decision and the phase plan.

## Layout

- `os/` is the bootc host image. `os/Containerfile` takes `GPU_VARIANT` (`amd`, `nvidia`, `intel`) and `IMAGE_TAG`.
- `os/rootfs/` is copied to `/` of the host. Quadlet units are in `os/rootfs/etc/containers/systemd/users/1001/`, and they run rootless as the `sibyl` user (UID 1001).
- `containers/` holds one directory per service image.
- `.github/workflows/bootc.yml` builds, pushes and signs all images.

## Rules

- Pin third-party images by digest, and pin GitHub Actions by commit SHA at the latest release.
- Fix linter findings. Do not add exclusions. Ask the maintainer if a finding cannot be fixed.
- `bootc container lint` must pass with no warnings.
- Every container drops all capabilities and sets `NoNewPrivileges=true`. Only Arcane runs with `SecurityLabelDisable=true`, because its Podman socket and D-Bus mounts need it.
- State under `/var` needs a `tmpfiles.d` entry, because bootc does not update `/var` after install.
- Users and groups come from `sysusers.d`. The `render` and `video` groups are copied into `/etc/group` at build time, so that sysusers can add members.

## Verify

```bash
docker buildx build --platform linux/arm64 -f os/Containerfile -t sibyl-os:test --load os
actionlint .github/workflows/bootc.yml
```

The arm64 build checks the Containerfile and runs `bootc container lint`. The amd64 images build in CI only, because the ROCm build needs more memory and disk than a laptop BuildKit VM has.
