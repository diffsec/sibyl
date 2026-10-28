# PXE install server

This container installs Sibyl over the network. It serves Fedora's signed shim and GRUB over TFTP, so Secure Boot stays on. It serves the Fedora installer and a kickstart over HTTP. The kickstart installs `ghcr.io/ihavespoons/sibyl-os` with `ostreecontainer`.

Warning: the default menu entry erases `INSTALL_DISK` with no confirmation, 10 seconds after the menu appears.

## Configuration

Put the settings in `tools/pxe/.env`. Git ignores that file.

| Variable | Default | Purpose |
|---|---|---|
| `PXE_SERVER_IP` | none | LAN address of the host that runs this container |
| `SSH_KEY` | none | Public key for the `admin` user |
| `INSTALL_DISK` | `nvme0n1` | Disk that the installer wipes |
| `TIMEZONE` | `Etc/UTC` | Installed system time zone |
| `HTTP_PORT` | `80` | Host port for the container HTTP server |
| `GRUB_KERNEL_VIA` | `http` | `http` or `tftp` for the kernel and initrd |

Set these DHCP options on the router:

- Option 66 (next-server): `PXE_SERVER_IP`
- Option 67 (boot file): `uefi/shimx64.efi`

On the box, choose "PXE Boot over IPv4". "HTTP Boot" needs a URL as the boot file, and it times out with this setup.

## Run on Linux

```bash
docker compose up --build -d
```

## Run on macOS

GRUB's HTTP client fails through the Docker port forwarder on macOS. GRUB connects, the server answers 200, and then GRUB reports that it must load the kernel first. A Linux host does not have this problem.

On macOS, use one of these:

- Set `GRUB_KERNEL_VIA=tftp`. This works, but the 251 MB initrd takes about 14 minutes.
- Set `HTTP_PORT=8080`, and serve the installer files from a native HTTP/1.1 server on port 80. With this, the initrd loads in under 40 seconds. Copy the files out of the container first:

```bash
mkdir -p www
docker compose exec -T pxe tar -C /data -cf - fedora44 srv/http/sibyl.ks | tar -xf - -C www
mv www/fedora44 www/fedora && mv www/srv/http/sibyl.ks www/ && rm -rf www/srv
cd www && python3 -c 'import http.server as h; h.SimpleHTTPRequestHandler.protocol_version = "HTTP/1.1"; h.ThreadingHTTPServer(("0.0.0.0", 80), h.SimpleHTTPRequestHandler).serve_forever()'
```

## After the install

1. Connect with `ssh admin@<box-ip>`.
2. Turn on Secure Boot in the firmware first. If the firmware is in Setup Mode, install its default keys. Make sure that `mokutil --sb-state` prints `SecureBoot enabled`.
3. Bind the encrypted root partition to the TPM, so that the box boots without a passphrase. The command asks for the current passphrase, which stays valid as the recovery key:

```bash
sudo systemd-cryptenroll --tpm2-device=auto --tpm2-pcrs=7 /dev/nvme0n1p3
```

Check the partition name with `lsblk` before you run it. PCR 7 records the Secure Boot state, so a binding made with Secure Boot off does not protect the disk, and it breaks when Secure Boot is turned on.

4. Reboot with `sudo systemctl reboot`. A staged `bootc upgrade` is applied only during a clean shutdown.

## Live network install

`live-netboot.sh` boots the Sibyl live system over the network and installs with no questions, through `sibyl-autoinstall.service`. It needs `ADMIN_PASSWORD` in `.env`, as well as `PXE_SERVER_IP` and `SSH_KEY`.

Warning: the default menu entry erases `INSTALL_DISK` with no confirmation, 10 seconds after the menu appears.

1. Run `tools/pxe/live-netboot.sh`. It pulls the ISO from GHCR. To use a local ISO, set `ISO_FILE=path/to.iso`.
2. Start the TFTP server: `PXE_MODE=live docker compose up --build -d`.
3. Serve `www` on port 80 with the native HTTP server that the script prints.
4. On the box, run `sudo efibootmgr -n <PXE IPv4 entry>` and reboot, or choose PXE in the firmware menu.

The live system prints the recovery key on its console and in the journal of `sibyl-autoinstall.service`. Read it before the reboot, or set `DEBUG_SSH=1` to let `SSH_KEY` log in as root to the live system. `DEBUG_SSH` changes only the live system in RAM, and it also lets you read the logs of a failed install.
