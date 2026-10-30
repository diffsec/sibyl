# Host settings

This guide shows you how to give other people access to the server, change its name and network, and run your own containers beside Sibyl.

You can change these settings in two places:

- The web console: the Host page.
- The `sibyl` TUI (the text console): the Host screen (`5`).

[Consoles](consoles.md) describes both. The steps below give the TUI keys.

| TUI key | Action |
|---|---|
| `h` | Change the hostname. |
| `n` | Change the network. |
| `a` | Add an operator. |
| `k` | Change an operator's SSH keys. |
| `d` | Delete an operator. |
| `p` | Make a model pool. See [Models](models.md#model-pools). |
| `x` | Detach a model pool. See [Models](models.md#model-pools). |

## Operators

An operator is a user account that can log in over SSH and run `sibyl`, but cannot use `sudo`. Operators are members of the `sibyl-operators` group.

An operator can read the System API key. Add only people whom you trust with the whole server.

### Add an operator

1. On the Host screen, press `a`. In the web console, open "Add an operator, or replace an operator's keys" on the Host page.
2. Type the user name.
3. Give the SSH public keys, one per line, or a GitHub user name. With a GitHub user name, Sibyl adds the keys that `github.com/<user>.keys` lists.
4. Save.

To replace an operator's keys, press `k`, or use the same form in the web console. To delete an operator and its home directory, press `d`, or use Delete in the web console.

### Let an existing account run `sibyl`

1. As an administrator, run:

   ```bash
   sudo usermod -aG sibyl-operators <user>
   ```

2. Tell the user to log out and back in.

## Hostname

1. On the Host screen, press `h`. In the web console, use the Hostname form on the Host page.
2. Type the new name. Use lowercase letters, digits and hyphens.
3. Save.

The TLS certificate keeps its names.

## Network

You can choose DHCP or a static IPv4 address for each network interface. A network change rolls back after 120 seconds unless you keep it. This protects you from a change that cuts off your connection.

1. On the Host screen, press `n`. In the web console, open "Change the network" on the Host page.
2. Choose the interface.
3. Choose DHCP or static.
4. For a static address, give the address with the prefix length, such as `192.168.1.26/24`.
5. For a static address, give the gateway. Leave it empty for no default route.
6. For a static address, give up to 3 DNS servers, separated by spaces.
7. Confirm the change.
8. If you connect over SSH or the web console and the address changed, connect again at the new address within 120 seconds. In the web console, sign in again.
9. Press `c` to keep the change. In the web console, choose "Keep the network change".

The TLS certificate keeps the address that it was made with. After an address change, use the name `sibyl.local`, or trust the browser warning again. [First boot](../getting-started/first-boot.md) shows how to trust the certificate.

## Run other containers with Arcane

Arcane is a web interface for containers. Use it to run other containers and Compose stacks beside Sibyl. Open it at `https://<address>:9443/`. [First boot](../getting-started/first-boot.md) shows how to get the Arcane password.

Arcane uses the same Podman as the Sibyl services, so it can also stop them. A Sibyl service that you stop starts again at the next boot.

The host accepts images from these public registries:

- Docker Hub
- Quay
- GitHub
- Google
- Microsoft
- Amazon ECR Public
- NVIDIA
- GitLab
- Chainguard
- LinuxServer

An administrator can allow another registry in `/etc/containers/policy.json`.

## Related

- [System API](../reference/system-api.md): the `/host` and `/pools` routes.
- [Recovery](recovery.md): disk encryption and the recovery key.
- [Updates](updates.md): update and roll back the host.
