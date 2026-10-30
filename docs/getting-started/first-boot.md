# First boot and setup

This page takes you from a freshly installed host to a working Sibyl server. You log in, run the setup wizard, trust the Sibyl certificate on your computers, and set the Arcane password. Do the [install](install.md) first.

## What the first boot does

The first boot creates the TLS certificate and the keys, then downloads the service images. This takes 5 to 15 minutes, depending on your internet connection. When the TPM is in use, the disk unlocks without a passphrase.

## Log in

You log in with the administrator account from the install.

1. Log in at the console, or over SSH from the second computer if you added a key:

   ```bash
   ssh admin@<address>
   ```

2. If you do not know the address, run `ip -br addr` at the console. You can also look in the list of DHCP clients on your router.

## NVIDIA key enrollment

Do this only on the `nvidia` variant. Skip it on AMD and Intel.

The NVIDIA driver is signed with the Sibyl Machine Owner Key (MOK). Secure Boot loads the driver only after you enroll that key once.

1. Run `sudo mokutil --import /usr/share/sibyl/sibyl-mok.der`. It asks you to choose a one-time password.
2. Reboot with `sudo systemctl reboot`.
3. A blue screen, the MOK manager, appears. Choose "Enroll MOK", then "Continue", then "Yes".
4. Type the one-time password.
5. The computer boots again, and the NVIDIA driver loads. Run `nvidia-smi` to check that it lists the GPUs.

## Run the setup wizard

The setup wizard walks through the first settings in order. It runs in two places with the same steps:

- In the terminal interface (TUI): run `sibyl setup`.
- In the web console: open `https://<address>:8443/console/setup` in a browser.

You can run the wizard again at any time. You can also change every setting later from the normal screens or pages. Each wizard step opens the normal screen or page for that setting.

### In the TUI

1. Run:

   ```bash
   sibyl setup
   ```

2. Follow each step. The step tells you which key to press.
3. Press `]` for the next step. Press `[` to go back.

### In the web console

1. Open `https://<address>:8443/console/setup` in a browser on the second computer.
2. Sign in with a host account in the `wheel` group, such as the administrator account from the install. Use the password of that account.
3. Select each step in the list. The list marks each step as "done" or "to do".

Until you [trust the certificate](#trust-the-certificate), the browser warns that the connection is not private. See [Consoles](../guides/consoles.md) for sign-in and sessions.

### The steps

| Step | TUI key | What you do |
|---|---|---|
| 1. GPUs, backend and layout | `e` | Choose the GPUs for inference, the backend, and the default layout. |
| 2. Hostname and network | `h`, `n` | Set the hostname, and a static address if you want one. |
| 3. Operator accounts and SSH keys | `a` | Add a person who can run `sibyl` over SSH. |
| 4. Open WebUI administrator | `o` | Create the first Open WebUI account. |
| 5. First model | `/` | Search Hugging Face and download a model. |
| 6. API keys for your programs | `n` | Make a key for each program that calls Sibyl. |
| 7. Done | | The TUI shows the addresses of the services and the CA certificate fingerprint. |

Step 1. The layout controls how models use the GPUs. "Span" splits one model across all the selected GPUs. "Pin" runs one model on each GPU, so several models can stay loaded at once. If you are not sure, keep the default backend and measure later with the benchmark. See [Models](../guides/models.md).

Step 2. In the TUI, press `h` for the hostname and `n` for the network. A network change rolls back by itself after 120 seconds unless you confirm it. Reconnect at the new address and press `c` to keep it.

Step 3. An operator can run `sibyl` but cannot use `sudo`. See [Host settings](../guides/host.md).

Step 4. Sign-up from the browser is off, so this step is the only way to make the first Open WebUI account. In the web console, you can also connect an existing Open WebUI administrator. Add more accounts later in Open WebUI, under Admin Panel, Users.

Step 5. The fit column shows which files suit your GPUs. See [Models](../guides/models.md).

Step 6. Make one key for each program, such as a laptop or Home Assistant. In the TUI, press `n` on the Router screen. In the web console, use the Router page. The new key shows once, so copy it into the program at once. See [API clients](../guides/api-clients.md) and [Router](../guides/router.md).

Programs can also use the default key in `/var/lib/sibyl-operator/inference.key`. The key in `api.key` is for the System API only.

Step 7. The TUI Status screen lists the addresses of Open WebUI, Arcane and the APIs as links. The last step also shows the SHA-256 fingerprint of the Sibyl CA certificate. You need this fingerprint in the next section.

## Trust the certificate

Trust the Sibyl CA once on each computer, and the browsers and programs stop warning. A certificate authority (CA) is a certificate that issues other certificates. The first boot makes the Sibyl CA, and the CA issues the TLS certificate of the server. The trust stays valid when the address or the hostname changes, because Sibyl issues a new server certificate from the same CA.

1. Copy the CA certificate from the host:

   ```bash
   scp admin@<address>:/var/lib/sibyl-operator/ca.crt sibyl-ca.crt
   ```

2. Show the fingerprint of the copy. It must match the fingerprint from the last wizard step:

   ```bash
   openssl x509 -in sibyl-ca.crt -noout -fingerprint -sha256
   ```

3. Add the certificate to the trusted authorities of your computer. Use the table below.

| System | What to do |
|---|---|
| macOS | Run `sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain sibyl-ca.crt`. |
| Fedora | Copy the file to `/etc/pki/ca-trust/source/anchors/` and run `sudo update-ca-trust`. |
| Debian and Ubuntu | Copy the file to `/usr/local/share/ca-certificates/sibyl-ca.crt` and run `sudo update-ca-certificates`. |
| Firefox | Firefox has its own list, under Settings, Privacy & Security, Certificates. |

### What the CA can vouch for

The CA can vouch only for local names and private addresses. Clients enforce these limits, so even a stolen CA key cannot vouch for a public site.

- Local names: `sibyl`, `localhost`, and names under `.local`, `.lan`, `.home.arpa` and `.internal`.
- Private addresses: `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10` and `127.0.0.0/8`.

The server certificate holds `sibyl.local`, `sibyl`, `localhost`, `<hostname>.local`, the hostname when it is a local name, and every private IPv4 address of the host. Sibyl issues a new server certificate at boot, within seconds of an address or hostname change, and before it expires.

A host from before 2026-10-29 had a self-signed certificate. Its first boot with this version replaces it. If you have such a host, trust the new `ca.crt` once.

## Set the Arcane password

Arcane runs other containers beside Sibyl. A new Arcane install has the login `arcane` / `arcane-admin`. The System API replaces that password with a random one within about 10 seconds of the first start of Arcane.

1. Open the Status screen of `sibyl`. It shows the new Arcane password.
2. Open `https://<address>:9443/` in a browser.
3. Log in with the user `arcane` and that password.
4. Choose your own password in Arcane.

After you change the password, the line on the Status screen goes away. See [Host settings](../guides/host.md) for more about Arcane.
