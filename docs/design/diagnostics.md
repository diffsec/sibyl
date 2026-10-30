# Diagnostics, watchdog and fleet design

This document describes failure capture, performance metrics, the hardware inventory and the inference watchdog on the Sibyl host. Diagnostic upload is opt-in. The upload path is the first part of a later fleet service for enterprise customers. [The design overview](overview.md) holds the rest of the design.

## Goals

- Capture enough data at the moment of an inference failure to find the cause without access to the box.
- Record tokens per second, GPU load, temperature, power and memory per model, so that a change to the OS, the backend or a model shows up as a number.
- Bring inference back without an operator after a hang, a crash or a full kernel lockup.
- Send nothing off the box unless the owner turns uploads on.
- Give every bundle a diagnostic ID that the owner can quote to support.

## Components

`sibyl-diag` (`cmd/sibyl-diag`, `internal/diag`) is one Go binary at `/usr/sbin/sibyl-diag`. `os/Containerfile` builds it with `sibyl-hostd`. It runs as root on the host, because it reads the kernel journal and debugfs, restarts the `sibyl` user's units, and can reboot.

| Unit | Command | Purpose |
|---|---|---|
| `sibyl-diag-boot.service` | `boot-check`, and `clean-shutdown` at stop | Finds an unclean shutdown, and sets safe mode after a boot loop. Runs before `user@1001.service`. |
| `sibyl-diag-metrics.service` | `metrics` | Samples the GPUs, the host and llama-server. Sandboxed, writes only `/var/lib/sibyl-diag`. |
| `sibyl-diag-watchdog.service` | `watchdog` | Probes inference and climbs the recovery ladder. |
| `sibyl-diag-upload.timer` | `upload-pending` | Sends queued bundles every hour, in automatic mode only. |

Host configuration that goes with it:

| File | Purpose |
|---|---|
| `usr/lib/systemd/system.conf.d/60-sibyl-watchdog.conf` | Hardware watchdog: `RuntimeWatchdogSec=60s`, `RebootWatchdogSec=5min` |
| `usr/lib/sysctl.d/61-sibyl-panic.conf` | `panic=10`, `panic_on_oops=1`, `softlockup_panic=1` |
| `usr/lib/systemd/journald.conf.d/60-sibyl.conf` | Persistent journal, 2 GB |
| `usr/share/sibyl/diagnostics.yaml` | Defaults. `/var/lib/sibyl/config/diagnostics.yaml` overrides them key by key. |

The watchdog and the sampler reach llama-swap on `127.0.0.1:18080`. `llama-swap.container` publishes that port on host loopback only. nftables accepts loopback traffic, and no rule forwards to the port, so remote clients still go through Caddy and the router. A local process can use the port without a router key. Operators and processes that run as `sibyl` can already reach llama-swap or queue host requests, so this design accepts that risk.

## Data on the box

`/var/lib/sibyl-diag` belongs to root, mode 0710 with group `sibyl`. The `sibyl` user can reach `out/` by name but cannot list the directory.

| Path | Content | Readers |
|---|---|---|
| `metrics/samples-YYYYMMDD.jsonl` | One sample per line: time, series, labels, value | root |
| `metrics/requests-YYYYMMDD.jsonl` | One llama-swap request per line | root |
| `incidents/<UTC time>-<reason>/` | One incident | root |
| `bundles/SBL-XXXX-XXXX.tar.gz` | Redacted bundles, with `.pending` and `.sent` markers | root |
| `watchdog.json` | Recovery history | root |
| `consent.json`, `device-id`, `redaction-salt` | Upload choice and device identity | root, mode 0600 |
| `out/status.json`, `out/perf.json` | Summary for the System API, written every minute | `sibyl`, labelled `container_file_t` |

The store is JSON lines, one file per day, not SQLite. The host binaries build with `CGO_ENABLED=0`, and the repo has no SQLite driver. Files older than `retention_days` (7) are deleted.

## Metrics

Every 10 seconds the sampler records:

| Source | Series |
|---|---|
| amdgpu sysfs (`gpu.Sample`) | Busy percent, VRAM and GTT use, edge, junction and memory temperature, power and power cap, fan, shader and memory clock |
| `nvidia-smi` | The same series for NVIDIA cards. Intel cards report none. |
| `/proc`, `statfs` | Load, free memory, swap use, free space on `/var/lib/sibyl` |
| llama-swap `/running` | Loaded models |
| llama-server `/metrics`, through `/upstream/<model>/metrics` | Every `llamacpp:*` series, plus `gen_tps` and `prompt_tps` from the counter changes |
| llama-swap `/api/metrics/activity` | One row per request: model, status, input, cached and output tokens, prompt and generation tokens per second, duration, error |

The speeds from the counters use busy seconds, not wall-clock time. They show the speed of the backend while it works, which is the number that a backend, quant or driver change moves. llama-swap keeps its request rows in memory, so its IDs start again at 1 after a restart. The sampler detects that and resets its cursor.

`sudo sibyl-diag perf -since 24h` prints requests, errors, and p50 and p5 generation speed per model. p5 is the slow tail: 95% of requests generated faster than it.

## Incidents

The watchdog captures an incident before it acts, because a restart or a reboot destroys most of the state. A failed capture step does not stop the others, and its error goes to `capture-errors.txt`.

| File | Content |
|---|---|
| `incident.json` | Reason, details, boot ID, device ID, version, GPU variant, image tag, safe mode |
| `journal-kernel.txt` | Kernel log of the current boot, with amdgpu ring timeouts and resets |
| `journal-system.txt`, `journal-sibyl-user.txt`, `journal-llama-swap.txt` | System, user-unit and llama-swap container logs, last 1 to 2 hours |
| `journal-previous-boot*.txt`, `pstore/` | After a crash: the log of the boot that crashed, and the kernel panic text |
| `llama-swap-logs.txt`, `llama-swap-state.json` | llama-swap and llama-server output, loaded models, last 200 requests, raw counters |
| `llama-swap-config.yaml` | The generated model commands: file, context, cache types, split, devices |
| `gpu-tools.txt` | `amd-smi`, `rocm-smi` or `nvidia-smi` output, when the host has the tool |
| `hardware.json` | The inventory below |
| `requests-1h.csv`, `samples-1h.jsonl`, `perf-24h.json` | Metrics around the failure |
| `units.txt`, `units-failed.txt`, `podman-ps.json`, `podman-images.json`, `watchdog-state.json` | Unit status, containers and image digests, recovery history |

The box keeps 50 incidents or 2 GB, whichever limit it reaches first. `sudo sibyl-diag capture -note "..."` records one by hand.

## Hardware inventory

`sudo sibyl-diag hardware` prints the Sibyl version, image tag and variant, OS release, kernel and command line, the bootc image and digest of the booted, staged and rollback slots, CPU model and relevant flags, memory, board and BIOS, each GPU from `gpu.Detect` with its VBIOS, disks, watchdog devices, pstore backend, EFI and Secure Boot state. It leaves out serial numbers, MAC addresses and the machine ID.

## Watchdog

### Probes

The watchdog runs every 30 seconds. A probe must fail 3 times in a row before the watchdog acts.

| Probe | Problem |
|---|---|
| `llama-swap.service` is `failed` | `service-failed` |
| `NRestarts` grew 3 times in 15 minutes | `crash-loop` |
| `GET /health` on llama-swap fails | `api-unresponsive` |
| `/upstream/<model>/health` or `/metrics` fails for a ready model | `model-unresponsive` |
| `requests_processing > 0`, and no change in generated tokens, prompt tokens or decode batches for 300 seconds | `generation-stalled` |
| A kernel line matches an amdgpu, NVIDIA Xid, i915, xe, soft-lockup, hung-task or OOM pattern | `gpu-fault`, capture only |

The watchdog does not probe while the unit starts or stops, because an image pull at start can take minutes. It does nothing while the unit is `inactive`, because an operator stopped it, for example from Arcane.

### Recovery ladder

`diag.Decide` picks the next step from the recovery history:

1. Restart llama-swap, at most 3 times in 15 minutes. A crash loop skips this step, because systemd already restarts the unit.
2. Reset each AMD GPU by reading `/sys/kernel/debug/dri/<n>/amdgpu_gpu_recover`, then restart llama-swap. Once in 15 minutes. If the read fails or takes over 60 seconds, the driver is stuck, and the watchdog goes to step 3.
3. Reboot, at most 2 times in 6 hours.
4. Stop acting and report `degraded`. The box stays up, so that an operator can read the state. The flag clears when the probes pass again.

`llama-swap.container` also sets `HealthOnFailure=kill` with `HealthRetries=3`. Podman kills a container that fails its health check, and `Restart=always` brings it back between watchdog probes.

### Full kernel lockup

A lockup stops the watchdog too. These parts handle it, in order:

1. `softlockup_panic` and `panic_on_oops` turn a soft lockup or an oops into a panic.
2. The kernel writes the panic text to pstore (EFI variables), and `panic=10` reboots 10 seconds later.
3. If the kernel cannot panic, the hardware watchdog resets the board. systemd pets `/dev/watchdog0` every 30 seconds with `RuntimeWatchdogSec=60s`. `RebootWatchdogSec=5min` covers a reboot that hangs on a stuck driver.
4. At the next boot, `systemd-pstore` archives the panic text to `/var/lib/systemd/pstore`.
5. `sibyl-diag-boot` finds the `running` marker that a clean shutdown removes. It records an `unclean-shutdown` incident with the journal of the crashed boot and the pstore record.
6. After more than 2 unclean boots in 6 hours, boot check writes `/var/lib/sibyl/config/safe-mode`. `llama-swap.container` has `ConditionPathExists=!` on it, so inference does not start, and the rest of the box comes up for diagnosis. `sudo sibyl-diag safe-mode off` clears it.

The flag is in the config directory, not in `/var/lib/sibyl-diag`, because the `sibyl` user manager evaluates the condition and cannot search root's directory. A failed `stat` would count as "no file", and llama-swap would start in safe mode.

The hardware watchdog needs a driver: `sp5100_tco` on AMD chipsets, `iTCO_wdt` on Intel. `sibyl-diag hardware` lists the devices. If the list is empty on a board, step 3 does not work there.

## Consent and privacy

Local capture is always on and stays on the box. Upload has three modes. Only `sudo sibyl-diag consent` writes the mode, to a root-only file, so an edit to `diagnostics.yaml` cannot turn uploads on.

| Mode | Behavior |
|---|---|
| `off` (default) | Nothing leaves the box. A bundle can still go to support on a USB drive. |
| `manual` | `sudo sibyl-diag submit` uploads one bundle after it shows the size and the endpoint and asks for confirmation. |
| `automatic` | The watchdog bundles each incident and queues it. `sibyl-diag-upload.timer` sends the queue every hour. |

The redactor runs on every file before it goes into a bundle. It removes private keys, bearer tokens, `X-Api-Key` values, Sibyl `sib-` keys, Hugging Face, OpenAI and GitHub tokens, `key=value` secrets and email addresses. It replaces IPv4, IPv6 and MAC addresses and the hostname with a keyed hash. The key is a random salt per device, so one address gives the same token in every file of that box, and the token cannot be reversed. Loopback and Podman container networks (`10.88.*`, `10.89.*`) stay as they are. A file with a NUL byte cannot be redacted, so it stays out.

Bundles never contain prompts or responses. llama-swap request rows hold token counts and speeds only. The router's decision log is not captured, because it names clients.

The device ID is a random UUID in `/var/lib/sibyl-diag/device-id`. It does not come from the machine ID or any serial number.

The upload endpoint must use HTTPS. Plain HTTP is accepted for loopback only, for tests.

## Diagnostic ID

A diagnostic ID has the form `SBL-XXXX-XXXX`, in Crockford base32, which has no I, L, O or U. Seven characters are random (35 bits), and the eighth is a check character with odd weights, so any single wrong character fails `diag.ValidID`. The box makes the ID when it builds a bundle, so the ID exists with or without an upload. The ingest service treats it as a proposal and returns the ID that it stored.

## Ingest service, API v1

The service is not built yet. `diagnostics.yaml` has an empty `endpoint`. The image sets it when the service exists, and `sudo sibyl-diag consent manual -endpoint https://...` overrides it per box.

```
POST {endpoint}/v1/bundles
Content-Type: application/gzip
X-Sibyl-Diagnostic-Id: SBL-7K2M-9QXD
X-Sibyl-Device-Id: <uuid>
X-Sibyl-Version: <sibyl version>
X-Content-Sha256: <hex SHA-256 of the body>

201 {"diagnostic_id": "SBL-7K2M-9QXD"}
```

The service must:

- Check the SHA-256 and the check character, and refuse a body over 50 MB.
- Store bundles encrypted at rest, keyed by diagnostic ID, with a retention limit. 90 days is the starting proposal.
- Rate-limit per device ID and per source address, because the endpoint takes anonymous uploads.
- Parse `manifest.json`, `hardware.json` and `perf-24h.json` into fields that support can search: GPU model and VBIOS, kernel, Sibyl version and image tag, backend, model file, failure reason. "Every gfx1100 ring timeout on tag X" is then one query.
- Let support look up a bundle by diagnostic ID, and delete every bundle of a device ID on request.

## Fleet service (long term)

The enterprise offering pairs Sibyl boxes with a cloud control plane for configuration, updates and support across many boxes. The device ID, the bundle format and the upload client above carry over unchanged.

### Pairing and transport

- The TUI shows a short pairing code. The admin enters it in the fleet console, and the service issues a client certificate bound to the device ID.
- The box connects out only, over mTLS. No inbound port opens, so a box behind NAT or a strict firewall works.
- Unpairing deletes the certificate on both sides and returns consent to the owner's choice.
- Pairing shows the data classes that the contract covers, and the owner accepts them on the box.

### Agent

A fleet agent on the host holds the connection. It reuses `internal/diag` for inventory, metrics and bundles, and the System API for configuration. It accepts commands from a fixed list, each with a typed payload, and checks each one as `sibyl-hostd` does:

| Command | Effect |
|---|---|
| `inventory`, `metrics` | Send the inventory and a metrics summary |
| `bundle` | Build and upload a bundle with an ID that support chose |
| `apply-hardware`, `apply-model` | `PUT /hardware` or `PUT /models/{id}` through the System API |
| `download-model` | `POST /downloads` |
| `upgrade` | `bootc switch --enforce-container-sigpolicy` to a signed image digest |
| `restart-service` | Restart one Sibyl unit through the host agent |
| `safe-mode` | Set or clear safe mode |

The agent has no remote shell. A support session that needs one is a separate, time-limited grant that the owner approves on the box.

### Fleet updates

- Rings: canary, early, broad. A ring gets a digest only after the ring before it stays healthy for a set time.
- Health is the watchdog's data: no `degraded` state, no unclean boots, and generation speed within a tolerance of the ring's baseline for the same GPU and model.
- A box that fails health after an upgrade runs `bootc rollback` and reports. greenboot can enforce this at boot (see "Open items").
- Offline sites get the same digests as offline bundles.

### Console

- Box list with version, GPUs, loaded models, health and speed.
- Incidents per box and across the fleet, grouped by failure reason and hardware.
- Configuration templates per group of boxes.
- An audit log of every command and who sent it.

## Phases

| Phase | Scope | Exit check |
|---|---|---|
| D1 | `sibyl-diag`: sampler, watchdog, boot check, incidents, bundles, consent, upload client. Hardware watchdog, panic settings, persistent journal. | Unit tests pass in CI. On the test host: `kill -STOP` on llama-server gives a `generation-stalled` incident and a restart. `echo c > /proc/sysrq-trigger` gives a reboot and an `unclean-shutdown` incident with a pstore record. `sibyl-diag hardware` lists a watchdog device. |
| D2 | System API routes that read `out/status.json` and `out/perf.json`, and host actions in `hostd.Decode` for bundle, submit, consent and safe mode. A TUI Diagnostics screen that shows speed per model, incidents and the latest diagnostic ID. The setup wizard asks for the consent mode. | An operator makes a bundle and reads its ID from the TUI, with no sudo. |
| D3 | Ingest service with API v1, and support lookup by diagnostic ID | A box in `manual` mode uploads, and support finds the bundle by ID. |
| D4 | Pairing, fleet agent, inventory and metrics in the console | Ten paired boxes show live health. |
| D5 | Remote configuration, model downloads, ring updates with rollback | A bad image on the canary ring rolls back with no person involved. |

## Open items

- D1 was tested with a fake sysfs, a fake llama-swap and a fake command runner only. The GPU reset, the hardware watchdog and pstore need a run on the test host before D1 closes.
- `kernel.panic_on_oops=1` reboots on any oops, including one that the box can survive. If it causes reboots on the test host, remove it and keep `softlockup_panic`.
- greenboot can roll back a bootc upgrade that fails a health check at boot, and a check can call `sibyl-diag status`. It needs a test with bootc on Fedora 44 first.
- kdump gives a full kernel dump but reserves memory at boot. pstore gives the panic text only. Decide after the first real crash if pstore is enough.
- Some model failures depend on the prompt. An opt-in "capture the failing request" mode needs llama-swap captures (`/api/captures/{id}` in v266) and its own consent level, because prompts can hold customer data.
- The watchdog restarts all of llama-swap. When llama-swap can restart one model process, the first step can unload only the failed model with `POST /api/models/unload/<model>`.
