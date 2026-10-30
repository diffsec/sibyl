# Updates

This guide shows how to update a Sibyl host, go back to the previous version, and update a host that has no internet.

Sibyl does not update by itself. The operating system and the services update together, as one image. An update downloads the new image and stages it. A staged version is downloaded and ready, and it starts at the next boot.

Your models, settings and accounts are in `/var`, which an update and a rollback do not change.

## Update from the web console

1. Open the Status page of the web console at `https://<address>:8443/console/status`.
2. Press "Check for an update".
3. If a version is available, the page shows it. Press "Apply update".
4. Choose when to reboot:
   - If "Reboot into it when it is staged" is on, the host reboots by itself when the download ends.
   - If it is off, the page offers "Reboot now" when the update is staged. Press it when you are ready.

The download runs in the background. Reload the page to see when an update step ends. A reboot stops the models and the APIs for a few minutes. If the new version does not boot, the host starts the old one, which bootc keeps.

## Update from the TUI

The Status screen of `sibyl` has the same controls as the web console.

| Key | Action |
|---|---|
| `u` | Check for an update. |
| `U` | Apply the available update. A form asks whether to reboot into it when it is staged. |
| `B` | Reboot into a staged update. |

If you choose not to reboot when you apply, press `B` later. The Status screen shows the staged version until the reboot.

## Update from a shell

1. Download and stage the new image:

   ```bash
   sudo bootc upgrade
   ```

2. Reboot into it:

   ```bash
   sudo systemctl reboot
   ```

`bootc upgrade` downloads the new image and checks its signature. The new version starts at the next boot. `sudo bootc status` shows the running version and the one that is staged.

## Roll back

If a new version has a problem, go back to the previous one:

```bash
sudo bootc rollback
sudo systemctl reboot
```

## Update offline

A host with no internet updates from a bundle on a USB drive. A bundle is a signed directory that holds the host image and every service image for one version and one variant.

1. On a computer with internet, download the bundle for your variant to a USB drive of 8 GB or more. This example is for `amd`:

   ```bash
   oras pull ghcr.io/diffsec/sibyl-bundle:latest-amd -o /path/to/usb/sibyl-bundle
   ```

2. Plug the drive into the Sibyl host.
3. Find the drive with `lsblk`.
4. Mount the drive. Replace `/dev/sdX1` with the partition of the drive:

   ```bash
   sudo mount /dev/sdX1 /mnt
   ```

5. Apply the bundle:

   ```bash
   sudo sibyl-update apply /mnt/sibyl-bundle
   ```

6. Reboot:

   ```bash
   sudo systemctl reboot
   ```

`sibyl-update` checks the signature of the bundle and every file before it changes anything. It refuses a bundle for another GPU variant. To only check a bundle, run `sibyl-update verify <directory>`.

CI builds a bundle only for release tags or on request, because a bundle is about 4 GB.

### Return to online updates

After an offline update, the host follows the bundle. When the host has internet again, run the command that `sibyl-update` prints. For the `amd` variant it is:

```bash
sudo bootc switch --enforce-container-sigpolicy ghcr.io/diffsec/sibyl-os:latest-amd
```

For the bundle format and how `sibyl-update` loads the images, see [the design](../design/overview.md).

## If an update fails

If an update step fails, the host keeps running the current version. The web console Status page says that the last update step failed. To see why, run this on the host:

```bash
journalctl -u sibyl-upgrade*
```

See also [Troubleshooting](troubleshooting.md).
