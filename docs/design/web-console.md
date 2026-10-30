# Web console design

This document gives contributors the design of the Sibyl web console (phase 7): why it exists, how it signs a browser in, how it stays in step with the TUI, and what is built. [Consoles](../guides/consoles.md) is the owner's guide to the console and the TUI.

Status on 2026-10-30: steps 7.1 to 7.4 are built, including the host update controls.

## Purpose and limits

The web console is a browser view of the `sibyl` TUI. It lets the owner manage the box from a laptop or a phone without SSH. It uses the System API, and it adds no new powers.

- The console does what the six TUI screens do: Status, Hardware, Models, Downloads, Host and Router (`internal/tui/app.go`). A change made in one shows in the other.
- The console calls the same System API routes as the TUI, through the same handlers. It adds no route that the TUI cannot use. A security check that the System API makes, such as the refusals in `internal/hostd`, holds for both.
- The console works on the LAN only, on the ports that Sibyl already opens. It adds no new firewall rule.
- The console does not replace Open WebUI for chat. It links to Open WebUI on port 443.
- The console does not show the System API key, a client key after its first display, or a provider key.

## Architecture

### Where it runs

The System API serves the console under the path `/console/` (`internal/console`). The templates, the stylesheet and htmx are embedded in the `sibyl-system-api` binary with `go:embed`. The console therefore has no container of its own and no extra image to pin.

Caddy has one route for it on port 18443, next to `/system/*`:

```caddyfile
handle /console/* {
	reverse_proxy system-api:3001
}
```

The owner opens `https://<address>:8443/console/`. The browser trusts the page if the owner installed the Sibyl CA certificate. Otherwise the browser shows a certificate warning, as it does for Open WebUI. After sign-in, `/console/` goes to the Router page.

### Technology

The console uses Go `html/template` pages on the server with htmx for partial updates. The design chose this over a single-page app in React or Svelte, for these reasons:

- The repository has no Node toolchain. A single-page app adds `package.json`, a lock file, a build step in CI and a stream of Dependabot updates for front-end packages.
- `html/template` escapes every value by context. A model name or a rule name from the owner cannot inject markup.
- htmx is one file of about 50 KB. It is vendored into the repository at a pinned version (`internal/console/static/htmx-2.0.4.min.js`), so the console loads no script from the internet.
- The page handlers call the API handlers in the same process, through the TUI's own `internal/client`, so the console and the TUI cannot drift apart. The client's transport goes to the API handler in memory, not over the network.

The pages use one stylesheet with a light and a dark theme from `prefers-color-scheme`. The layout works at phone width. Live views, such as download progress and the router's last decisions, refresh every 5 seconds with `hx-trigger="every 5s"`, as the TUI does.

The model settings form of the console and the TUI's form share `internal/modelform`, so both check a value the same way.

## Components

### Sign-in and sessions

The System API key is a bearer token for programs. A browser must not keep it in JavaScript memory or in local storage, because a script injection can read it there. The console uses a session cookie instead:

1. The sign-in page posts a user and a password to `POST /console/login`.
2. The System API checks the password (see [The password check](#the-password-check)). It creates a session with a random 32-byte ID. It keeps the session in memory, so a restart signs everyone out.
3. The response sets a cookie `sibyl_console` with `Secure`, `HttpOnly`, `SameSite=Strict` and `Path=/console/`. A session ends after 12 hours, or after 1 hour without a request.
4. Every console request that changes something must carry an `Origin` header that matches the host. This check and `SameSite=Strict` stop a cross-site request.
5. The sign-in route allows 5 tries per minute per browser address. A try counts before its check runs, so parallel requests cannot pass the limit, and at most 2 checks run at once. The address comes from Caddy's `X-Forwarded-For`, which the console trusts only from Caddy's own address. Each sign-in and each failure goes to the System API log.
6. An htmx poll does not refresh a session, so a page left open still ends after 1 hour without use. A message in the URL shows only on a signed-in page.

### The password check

The sign-in secret is the host `admin` password. `sibyl-hostd` checks it with a separate action, because the System API runs as `sibyl`, and a password check needs root.

`sibyl-hostd` has a second queue for the password checks, `/run/sibyl-auth/req`.

- The queue is in memory, so a password never reaches a disk.
- The `sibyl` user owns it, as it owns the disk queue, so the System API container can mount it with `:z`.
- `sibyl-hostd-auth.path` starts `sibyl-hostd -auth` when a request arrives.
- The disk queue refuses the action `password-check`, because `hostd.Decode` does not know it. The memory queue refuses every other action.

The check passes when the account is in the `wheel` group and `unix_chkpwd`, PAM's own helper, accepts the password. Every failure gives the same error, so a check does not tell whether an account exists.

`unix_chkpwd` run as root skips the PAM stack, so `sibyl-hostd` adds what PAM gives:

- It refuses an account whose expiry date has passed.
- It waits 1 second after each failure.
- After 10 failures for one account in 10 minutes, it refuses that account's checks without a try.
- It keeps the failures in `/var/lib/sibyl-host`, which only root can write. A compromised System API therefore cannot reset them by writing to the queue directly.

A request with an invalid name gets no result file. The result holds only the outcome. It goes to `/var/lib/sibyl-host/out/auth/` and is deleted after one minute.

The unit `sibyl-hostd-auth.service` has no network, a read-only system and four capabilities. It does not set `NoNewPrivileges`, because with it SELinux can refuse the switch to `unix_chkpwd`'s domain, and every sign-in fails. It does not set `RestrictSUIDSGID`, because that seccomp filter refuses `openat2` with `ENOSYS`, and the queue reader uses `openat2`. On 2026-10-30 on the test host, with SELinux enforcing, a wrong password gave 401 and the right one a session.

### Pages

The console has the pages Router, Models, Downloads, Hardware, Host, Status and Setup.

| Page | Shows | Controls |
|---|---|---|
| Status | Version, update state, services, GPU memory, the loaded models | Restart a service, connect Open WebUI, check for an update, apply an update with a reboot, reboot |
| Hardware | GPUs, the selected GPUs, the backend, memory | Select GPUs, select the backend |
| Models | Installed models with size, fit, context, layout and benchmark | Model settings (the TUI settings form), benchmark, delete |
| Downloads | Hugging Face search, the queue with progress | Search, pick a file, start, cancel, retry, set the Hugging Face token |
| Host | Hostname, network, users, SSH keys, model pools | Change each, with the same checks as `sibyl-hostd` |
| Router | Clients, providers, profiles and rules, calibration, last decisions | Every control of the TUI Router screen, including `m`, `r`, `c`, `s`, `a` and `C` |
| Setup | The steps of `sibyl setup` | A link to the page of each step |

The Router page shows the calibration cells per local model as a table. The rules move with up and down buttons, which need no script.

The Setup page lists the six steps of `sibyl setup` in the same order. Each step shows whether it is done, from the System API's state, and links to its page. The [overview](overview.md#setup-wizard) lists the steps.

### Network changes

A network change shows a warning with the rollback time. If the address changes, the owner opens the console at the new address, signs in again, and keeps the change there, because the session cookie belongs to the old address. The [overview](overview.md#privileged-host-actions) describes the NetworkManager checkpoint and the rollback.

### Host updates

The Status page checks for, downloads and applies a new host image through three `sibyl-hostd` actions. Each action starts a unit that runs bootc. The [overview](overview.md#online-updates) describes the units and the host state. The TUI Status screen has the same controls.

## Security

- The Content Security Policy is `default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; frame-ancestors 'none'; form-action 'self'; base-uri 'none'`. No page has an inline script or an inline style.
- A delete, a reboot, an update and a key change ask for confirmation in the page. The confirmation names the thing that changes.
- A new client key shows once, in the response to its creation, as in the TUI.
- The console logs each change that works with the user name. Each sign-in, sign-out and failed sign-in goes to the log with the user and the browser address.
- A security review on 2026-10-30, before the first image that ships the console, covered the sessions, the `Origin` check, the templates and the password check. It found no critical bug. Its 2 medium and 5 low findings are fixed: the limits above, the session poll, the URL message and the result names. The fix for the check unit was `NoNewPrivileges`. A later change took it out again, for the SELinux reason in [The password check](#the-password-check).
- One risk stays: a device on the LAN can lock the owner's account out of the console for 10 minutes with 10 wrong passwords. SSH and the TUI still work.

## Delivery

| Step | Scope | Exit check | Status |
|---|---|---|---|
| 7.1 | Sign-in, sessions, the Caddy route, the page frame, and the Status screen as read-only | A request without a session gets the sign-in page. A request with a session from another `Origin` gets 403. The Status screen matches the TUI. | Built |
| 7.2 | The Router screen with every control | Each Router control of the TUI works in the console, and a change in one shows in the other within 5 seconds. | Built |
| 7.3 | The Models and Downloads screens | A model downloads, gets its settings and runs from the console alone. | Built |
| 7.4 | The Hardware and Host screens, and the update | A fresh install can be set up from the console after the TUI setup wizard. | Built |

## Decisions

The owner chose on 2026-10-30:

1. The sign-in secret is the host `admin` password, which a new `sibyl-hostd` action checks. The other choice was the System API key, which the owner must copy from the host. The password won because the owner already knows it and a phone can type it.
2. The pages are Go templates with htmx.
3. The Router screen comes first after sign-in.
4. The setup wizard runs in both the TUI and the console.

## Open items

- A device on the LAN can lock the owner out of the console for 10 minutes (see [Security](#security)).
