# Sibyl documentation

The pages are grouped by what you want to do.

## Getting started

| Page | What it covers |
|---|---|
| [Install](getting-started/install.md) | What you need, the installer, preparing the computer, installing, and trying Sibyl from the USB stick without installing |
| [First boot](getting-started/first-boot.md) | Logging in, the NVIDIA key, the setup wizard in the terminal or the browser, trusting the certificate |

## Guides

| Page | What it covers |
|---|---|
| [Consoles](guides/consoles.md) | The web console and the `sibyl` terminal console |
| [API clients](guides/api-clients.md) | Keys for your programs, and calls to the OpenAI, Anthropic and Ollama APIs |
| [Models](guides/models.md) | Downloading models, model settings, presets, reasoning, benchmarks and model pools |
| [Router](guides/router.md) | Clients and keys, cloud providers, budgets, routing rules and calibration |
| [Host](guides/host.md) | Operator accounts, the network, and other containers with Arcane |
| [Updates](guides/updates.md) | Updating online, rolling back, and offline update bundles |
| [Recovery](guides/recovery.md) | Disk encryption, the recovery key, and a TPM that does not unlock the disk |
| [Troubleshooting](guides/troubleshooting.md) | Services that do not start, models that do not load, and reporting a problem |

## Reference

| Page | What it covers |
|---|---|
| [System API](reference/system-api.md) | Every System API route, with its body and response |
| [Configuration](reference/configuration.md) | Every model setting and router setting, and the files that hold them |

## Design

These pages are for contributors. They explain how Sibyl works and why.

| Page | What it covers |
|---|---|
| [Overview](design/overview.md) | The decisions, the host image, the services, storage, security and the phase plan |
| [Router](design/router.md) | The model router, the classifier, calibration and their measurements |
| [Web console](design/web-console.md) | The web console, its sign-in and its security |
| [Development](development.md) | Building, testing, CI, releases and testing on hardware |

## Legal

- [Individual Contributor License Agreement](legal/individual-cla.md)
- [Corporate Contributor License Agreement](legal/corporate-cla.md)
