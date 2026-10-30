# Troubleshooting

This guide lists common problems and how to fix them. If your problem is not here, see [Report a problem](#report-a-problem).

## A service does not start

The Status screen in `sibyl` and the Status page of the web console show each service.

1. Look at the state and the log of the service. As the administrator, run these commands. Replace `llama-swap` with the service:

   ```bash
   sudo systemctl --user -M sibyl@ status llama-swap
   sudo journalctl _SYSTEMD_USER_UNIT=llama-swap.service -n 50
   ```

2. To restart the service, select it on the Status screen and press `r`.

The services are `llama-swap`, `router`, `open-webui`, `system-api`, `caddy` and `arcane`.

## A model does not load

1. Look at the llama-swap log with the commands in [A service does not start](#a-service-does-not-start).
2. If the log shows an "out of memory" error, the model or its context size is too large for the selected GPUs. Choose a smaller quantization or a smaller context size. See [Models](models.md).
3. On AMD, if the HIP backend fails on a card that Sibyl does not recognize, change the backend to Vulkan on the Hardware screen.

Sibyl refuses HIP for the cards that it knows the HIP image does not support.

## `sibyl` says that you are not in the sibyl-operators group

1. An administrator adds you to the group:

   ```bash
   sudo usermod -aG sibyl-operators <user>
   ```

2. Log out and log in again.

## The web console refuses your sign-in

The web console shows "Wrong user or password. The user must be in the wheel group." The web console accepts only host accounts in the `wheel` group, such as the administrator from the install. Operators cannot sign in to the web console. Sign in with an administrator account. See [Consoles](consoles.md).

## The browser cannot reach Sibyl

1. Make sure that you use `https://`.
2. Make sure that you use the right port. The table below lists the ports.
3. The host firewall allows only these ports and SSH. Make sure that no firewall between your computer and the host blocks them.

| Service | Port |
|---|---|
| Open WebUI | None (443) |
| APIs and the web console | 8443 |
| Arcane | 9443 |

## The browser warns about the certificate

The browser does not trust the Sibyl CA yet. Trust the CA once on each computer. See [Trust the certificate](../getting-started/first-boot.md#trust-the-certificate).

## An update fails

The host keeps running the current version. See [If an update fails](updates.md#if-an-update-fails). If the new version boots but has a problem, see [Roll back](updates.md#roll-back).

## The boot asks for a passphrase

The TPM did not unlock the disk. Type the recovery key. See [Disk encryption and recovery](recovery.md).

## Report a problem

Open an issue at [github.com/diffsec/sibyl/issues](https://github.com/diffsec/sibyl/issues). Include these items:

- The output of `sudo bootc status`.
- The Status screen.
- The log of the service that fails.

Do not include a key.
