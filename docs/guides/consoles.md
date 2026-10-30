# The consoles

Sibyl has two consoles for managing the host. Both use the System API, so a change in one shows in the other.

- The `sibyl` TUI is a terminal interface. You run it on the host, at the console or over SSH.
- The web console is a set of pages in the browser at `https://<address>:8443/console/`. You can use it from a laptop or a phone without SSH.

Neither console shows the System API key, a client key after its first display, or a provider key. Chat is not part of either console. Use Open WebUI at `https://<address>/` for chat.

## The sibyl TUI

Run `sibyl` on the host. The TUI has six screens.

| Key | Action |
|---|---|
| `1` to `6` | Go to that screen |
| `tab`, `shift+tab` | Go to the next or the previous screen |
| `q` | Quit |
| `ctrl+c` | Quit, also while a form is open |

The bottom line of each screen lists its keys. While a form is open, `enter` goes to the next field, `shift+tab` goes back, and `esc` discards the form.

### Screens and keys

| Key | Screen | Keys on the screen |
|---|---|---|
| `1` | Status | `r` restarts the selected service. `o` connects Open WebUI with an admin key. `u` checks for an update. `U` applies it. `B` reboots into a staged update. |
| `2` | Hardware | `e` changes the GPUs, the backend and the default layout. |
| `3` | Models | `enter` edits the settings of the selected model. `b` benchmarks the backends. `d` deletes the model. |
| `4` | Download | `/` searches Hugging Face. `t` sets a Hugging Face token. `c` cancels a download. `R` retries one. |
| `5` | Host | `h` hostname. `n` network. `a` adds an operator. `p` makes a model pool. `x` detaches a pool. For a selected operator, `k` changes the SSH keys and `d` deletes the operator. After a network change, `c` keeps the change. |
| `6` | Router | `n` new client and key. `e` edits a client. `p` adds or changes a provider. `x` deletes a provider. `m` profiles. `r` rules. `c` calibration. `s` sensitive words and classifier. For a client that is not built in, `k` makes a new key and `d` deletes the client. `a` applies a calibration proposal, when one exists. `C` deletes the calibration samples, when some exist. |

Use `↑` and `↓` to select a row in a list.

The Status screen shows the addresses, the GPU memory and the state of each service. The Router screen shows the spend per client and the last decisions. [Use the router](router.md) explains each Router key.

The other guides explain the work on each screen:

- Models and downloads: [Models](models.md).
- Hostname, network, operators and pools: [Host](host.md).
- Updates: [Updates](updates.md).

### The setup wizard

`sibyl setup` starts the TUI as a wizard. The wizard goes through the setup steps in order, and each step opens the normal screen for it. Press `]` for the next step and `[` to go back. You can run `sibyl setup` again at any time. [First boot](../getting-started/first-boot.md) describes each step.

## The web console

Open `https://<address>:8443/console/` in a browser. If your computer does not trust the Sibyl certificate authority, the browser shows a certificate warning. [First boot](../getting-started/first-boot.md) explains how to trust it.

### Sign in

1. Open `https://<address>:8443/console/`.
2. Type the user name and the password of a host account in the `wheel` group. The admin account from the install is one.
3. Select Sign in.

The console opens on the Router page.

If the user or the password is wrong, the page says "Wrong user or password. The user must be in the wheel group." The message is the same when the account does not exist.

Sign-in has these limits:

- After 5 failed sign-ins from one address in a minute, that address must wait a minute.
- After 10 failed sign-ins for one account in 10 minutes, the host refuses that account until those failures are 10 minutes old. SSH and the TUI still work in that time.
- An account whose expiry date has passed cannot sign in.

### Sessions

A session ends after 12 hours, or after 1 hour without use. A page that refreshes itself does not count as use, so a page left open still ends after 1 hour. A restart of the System API ends every session.

To end a session yourself, select Sign out.

If a network change gives the host a new address, open the console at the new address and sign in again. The session cookie belongs to the old address.

### Pages

| Page | What you can do |
|---|---|
| Router | Clients and keys, providers, profiles and rules, calibration settings, sensitive words and the classifier, calibration results and proposals, and the last decisions. See [Use the router](router.md). |
| Models | See the installed models. Edit a model's settings, run the benchmark, or delete a model. See [Models](models.md). |
| Downloads | Search Hugging Face, start a download, cancel or retry one, and set or remove the Hugging Face token. See [Models](models.md). |
| Hardware | Select the GPUs, the backend and the default layout. |
| Host | Change the hostname and the network, add or delete operators and their SSH keys, and make or detach model pools. See [Host](host.md). |
| Status | See the services, restart one, connect Open WebUI, check for an update, apply it, and reboot. See [Updates](updates.md). |
| Setup | See the six steps of `sibyl setup` in the same order. Each step shows whether it is done and links to its page. |

The live parts of a page, such as download progress and the router's last decisions, refresh every 5 seconds.

A delete, a reboot, a disk erase and a new client key sit behind a collapsed section, so each takes two clicks. The second button names the thing that changes, such as "Delete rule secrets". Making a model pool also asks you to type the disk's serial number.

[Web console design](../design/web-console.md) explains how the console works.
