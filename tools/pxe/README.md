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
2. Bind the disk to the TPM, so that the box boots without a passphrase:

```bash
sudo systemd-cryptenroll --tpm2-device=auto --tpm2-pcrs=7 /dev/nvme0n1p3
```

Check the partition name with `lsblk` before you run it.
