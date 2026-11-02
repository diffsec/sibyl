# Diagnostics

This guide shows how to read the watchdog state, send a problem to support, and leave safe mode. You can do each task in three places with no difference in result: the Diagnostics screen of the `sibyl` TUI (key `7`), the Diagnostics page of the web console, or `sibyl-diag` with `sudo`. The TUI and the console need no `sudo`.

Sibyl watches inference and brings it back after a failure. It restarts llama-swap, then resets AMD GPUs, then reboots. After each failure it saves an incident with the logs, the GPU state and the speed of each model. Incidents stay on the host. [Diagnostics design](../design/diagnostics.md) describes how it works.

## Show the state

Show the watchdog state, the upload choice and safe mode:

```bash
sudo sibyl-diag status
```

Show the requests and the generation speed of each model over the last day:

```bash
sudo sibyl-diag perf -since 24h
```

## Send a problem to support

In the TUI, open the Diagnostics screen and press `b`. Choose the selected incident or a new snapshot, and add a note. The screen then shows the latest diagnostic ID. In the web console, use "Make a bundle" on the Diagnostics page. With `sudo`, do these steps:

1. Make a bundle. The command saves an incident now, removes keys, addresses and the hostname, and prints a diagnostic ID such as `SBL-7K2M-9QXD`:

   ```bash
   sudo sibyl-diag bundle -note "what you saw"
   ```

   To bundle an incident that the watchdog saved, use `-latest`, or list them with `sudo sibyl-diag incidents`.
2. Give the diagnostic ID to support.
3. Send the bundle file from `/var/lib/sibyl-diag/bundles/`, or upload it as below.

Sibyl sends nothing unless you turn uploads on. `manual` sends a bundle only when you run `submit`, and asks first. `automatic` sends every incident:

```bash
sudo sibyl-diag consent manual -endpoint https://support.example.com
sudo sibyl-diag submit
```

The Sibyl ingest service does not exist yet, so the image has no default endpoint. Use the endpoint that your support provider gives you.

The TUI and the console can set the consent mode and send a bundle, but they cannot set the endpoint. Only an administrator sets it with `sudo`. An operator therefore cannot send bundles to a server of their own choice.

`sudo sibyl-diag consent off` turns uploads off again.

## Safe mode

If the host crashes more than twice in 6 hours, Sibyl starts without llama-swap, so that you can read the incidents. The rest of the host works as usual. To leave safe mode, press `m` on the TUI Diagnostics screen, or use the Diagnostics page of the web console. Then start llama-swap on the Status screen. With `sudo`, run:

```bash
sudo sibyl-diag safe-mode off
sudo systemctl --user -M sibyl@ start llama-swap.service
```
