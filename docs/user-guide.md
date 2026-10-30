# Sibyl user guide

This guide takes you from a computer with a GPU to a private AI server that answers chat and API requests. It covers the install, the first setup, daily use, updates and recovery. You do not need to know Linux well, but you must be able to change firmware settings and use a terminal.

Sibyl is pre-alpha. The AMD variant is tested on one host with two Radeon RX 7900 XTX cards. The NVIDIA and Intel variants build and pass the same checks, but no one has run them on real hardware yet.

## Contents

1. [What you need](#what-you-need)
2. [Get the installer](#get-the-installer)
3. [Prepare the computer](#prepare-the-computer)
4. [Install Sibyl](#install-sibyl)
5. [First boot and setup](#first-boot-and-setup)
6. [Use Sibyl](#use-sibyl)
7. [Models](#models)
8. [Host settings and accounts](#host-settings-and-accounts)
9. [Run other containers with Arcane](#run-other-containers-with-arcane)
10. [Updates](#updates)
11. [Disk encryption and recovery](#disk-encryption-and-recovery)
12. [Try Sibyl from the USB without an install](#try-sibyl-from-the-usb-without-an-install)
13. [Troubleshooting](#troubleshooting)

## What you need

| Item | Requirement |
|---|---|
| Computer | x86-64 with UEFI firmware. Secure Boot is recommended. |
| TPM | A TPM 2.0 chip, or the firmware TPM (AMD fTPM or Intel PTT). Without one, you type a passphrase at every boot. |
| GPU | One or more GPUs from one vendor. See the table below. |
| Disk | One internal disk of 64 GiB or more. The installer erases it. Models need more space, so plan for the models you want. |
| USB stick | 8 GB or more for the installer. 32 GB or more if you want to run Sibyl from the stick. |
| Network | Wired Ethernet with DHCP and internet access. The first boot downloads the service images, a few GB. |
| Second computer | To download the installer, write the USB stick, and later use Sibyl from a browser. |

Each GPU vendor has its own Sibyl image, called a variant. Pick the variant that matches your GPUs.

| Variant | GPUs | Backends |
|---|---|---|
| `amd` | Radeon and Ryzen AI. HIP runs on RX 6800 and 6900, RX 7000, RX 9000, Radeon PRO W6800, W7000 and AI PRO R9700, and Ryzen AI 300 and Strix Halo APUs. Other Radeon cards use Vulkan. | HIP (ROCm), Vulkan |
| `nvidia` | GeForce RTX 20 series and newer, and data-center GPUs from Turing on | CUDA, Vulkan |
| `intel` | Arc and recent integrated graphics | Vulkan |

A backend is the GPU code that llama.cpp uses. Sibyl can measure which backend is faster on your hardware.

## Get the installer

The installer is an ISO file. It is published as an OCI artifact on GitHub's container registry, because it is too large for a GitHub release.

1. Install `oras` and `cosign` on the second computer. See [oras.land](https://oras.land/docs/installation) and [docs.sigstore.dev](https://docs.sigstore.dev/cosign/system_config/installation/).
2. Find the ISO for your variant, and check its signature with the Sibyl public key. `cosign` must print `The signatures were verified against the specified public key`. This example is for `amd`:

```bash
DIGEST="$(oras resolve ghcr.io/diffsec/sibyl-iso:latest-amd)"
cosign verify --new-bundle-format=false \
  --key https://raw.githubusercontent.com/diffsec/sibyl/main/os/rootfs/etc/pki/containers/sibyl-cosign.pub \
  "ghcr.io/diffsec/sibyl-iso@$DIGEST"
```

The key signature uses cosign's older format, because podman and bootc read only that format. cosign 3 needs `--new-bundle-format=false` to find it, and prints that the flag is deprecated. ISOs built before 2026-10-29 have no key signature.

3. Download that ISO. `oras` checks each file against the digest that the signature covers:

```bash
mkdir sibyl-iso && cd sibyl-iso
oras pull "ghcr.io/diffsec/sibyl-iso@$DIGEST"
```

4. Make sure that the file is complete. The command must print `OK`. On Linux, use `sha256sum -c SHA256SUMS` instead:

```bash
shasum -a 256 -c SHA256SUMS
```

The ISO file name contains the build tag, such as `sibyl-sha-339b7a4-amd.iso`. The examples below write it as `sibyl-*.iso`.

5. Write the ISO to the USB stick. This erases the stick.

On Linux, find the stick with `lsblk`, then write the ISO. Replace `/dev/sdX` with the stick, not with a disk that you need:

```bash
sudo dd if=sibyl-*.iso of=/dev/sdX bs=4M conv=fsync status=progress
```

On macOS, find the stick with `diskutil list`, then:

```bash
diskutil unmountDisk /dev/diskN
sudo dd if=sibyl-*.iso of=/dev/rdiskN bs=4m
```

On Windows, use [balenaEtcher](https://etcher.balena.io/), or Rufus in DD image mode.

## Prepare the computer

Make these changes in the firmware setup. The key to open it is usually Del, F2 or F10 at power-on.

1. Turn on Secure Boot. If the firmware is in Setup Mode, install the default keys first.
2. Turn on the TPM. It is often called fTPM, PTT or "Security Device Support".
3. Set the boot order to USB first, or note the key for the one-time boot menu, often F8, F11 or F12.
4. Set a firmware password, and lock the boot order. The TPM unlocks the disk for any system that boots through the same signed boot loader, so a locked firmware is part of the protection.

NVIDIA only: the NVIDIA driver is signed with the Sibyl project key. Secure Boot loads it only after you enroll that key. You do this after the install, in [NVIDIA key enrollment](#nvidia-key-enrollment).

## Install Sibyl

1. Plug in the USB stick and the network cable. Boot from the stick.
2. The live system starts and shows a menu on the screen:

```
  1  Install Sibyl to a disk
  2  Make a persistence partition on this USB
  3  Setup wizard (needs persistence)
  4  Shell
  5  Reboot
  6  Power off
```

3. Type `1` and press Enter. The installer asks these questions:

| Question | What to enter |
|---|---|
| Disk | The internal disk for Sibyl. The installer hides the USB stick and disks that are too small or in use. |
| Administrator user name | The account that you log in with. It can use `sudo`. The default is `admin`. |
| Password | At least 8 characters. You use it at the console and for `sudo`. |
| SSH public keys | Optional. One key per line, such as the content of `~/.ssh/id_ed25519.pub`. SSH accepts keys only, not passwords. |
| GitHub user | Optional. The installer adds the public keys from `https://github.com/<user>.keys`. |
| Disk passphrase | Optional when the computer has a TPM. Required without one. At least 12 characters. |

4. The installer shows the disk and asks you to type its serial number. This is the last chance to stop. Press Ctrl+C to cancel.
5. The install takes a few minutes. At the end, the installer prints the recovery key, eight groups of eight letters:

```
    cbdefghi-jklnrtuv-cbdefghi-jklnrtuv-cbdefghi-jklnrtuv-cbdefghi-jklnrtuv
```

Write the recovery key down and keep it away from the computer. You need it if the TPM refuses to unlock the disk, for example after a firmware update. Without it and without a passphrase, the data on the disk is lost.

6. Confirm that you wrote down the key. The computer reboots. Remove the USB stick.

## First boot and setup

The first boot creates the TLS certificate and the keys, then downloads the service images. This takes 5 to 15 minutes, depending on your internet connection. The disk unlocks without a passphrase when the TPM is in use.

### Log in

Log in at the console with the administrator account, or over SSH from the second computer if you added a key:

```bash
ssh admin@<address>
```

To find the address, run `ip -br addr` at the console, or look in your router's list of DHCP clients.

### NVIDIA key enrollment

Skip this section on AMD and Intel.

1. Run `sudo mokutil --import /usr/share/sibyl/sibyl-mok.der`. It asks you to choose a one-time password.
2. Reboot with `sudo systemctl reboot`.
3. A blue screen, the MOK manager, appears. Choose "Enroll MOK", then "Continue", then "Yes", and type the one-time password.
4. The computer boots again, and the NVIDIA driver loads. `nvidia-smi` lists the GPUs.

### Run the setup wizard

Run:

```bash
sibyl setup
```

The wizard walks through the setup screens in order. Each step tells you which key to press. Press `]` for the next step and `[` to go back. You can run `sibyl setup` again at any time, and you can change every setting later from the normal screens.

1. GPUs, backend and layout. Press `e`. Choose the GPUs for inference, the backend, and the default layout. "Span" splits one model across all the selected GPUs. "Pin" runs one model on each GPU, so several models can stay loaded at once. If you are not sure, keep the default backend and measure later with the benchmark.
2. Hostname and network. Press `h` for the hostname and `n` for the network. A network change rolls back by itself after 120 seconds unless you confirm it. Reconnect at the new address and press `c` to keep it.
3. Operator accounts. Press `a` to add a person who can run `sibyl` over SSH. Operators cannot use `sudo`.
4. Open WebUI administrator. Press `o` and create the first Open WebUI account. Sign-up from the browser is off, so this is the only way to make the first account. Add more accounts later in Open WebUI, under Admin Panel, Users.
5. First model. Press `/` and search Hugging Face. See [Download a model](#download-a-model).
6. API keys for your programs. On the Router screen, press `n` for each program that will call Sibyl, such as a laptop or Home Assistant. The new key is shown once. Copy it into the program. See [The API keys](#the-api-keys).
7. Done. The Status screen lists the addresses of the services as links. The last step also shows the SHA-256 fingerprint of the Sibyl CA certificate.

### Trust the certificate

Sibyl has its own certificate authority (CA). The first boot makes the CA, and the CA issues the server's TLS certificate. Trust the CA once on each computer, and the browsers and programs stop warning. The trust stays valid when the address or the hostname changes, because Sibyl issues a new server certificate from the same CA.

1. Copy the CA certificate from the host:

```bash
scp admin@<address>:/var/lib/sibyl-operator/ca.crt sibyl-ca.crt
```

2. Make sure that it is the right one. The fingerprint must match the one from the last wizard step:

```bash
openssl x509 -in sibyl-ca.crt -noout -fingerprint -sha256
```

3. Add it to the trusted authorities. On macOS: `sudo security add-trusted-cert -d -r trustRoot -k /Library/Keychains/System.keychain sibyl-ca.crt`. On Fedora: copy it to `/etc/pki/ca-trust/source/anchors/` and run `sudo update-ca-trust`. On Debian and Ubuntu: copy it to `/usr/local/share/ca-certificates/sibyl-ca.crt` and run `sudo update-ca-certificates`. Firefox has its own list, under Settings, Privacy & Security, Certificates.

The CA can vouch only for local names (`sibyl`, `localhost`, and names under `.local`, `.lan`, `.home.arpa` and `.internal`) and private addresses (`10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `100.64.0.0/10` and `127.0.0.0/8`). Clients enforce these limits, so even a stolen CA key cannot vouch for a public site.

The server certificate holds `sibyl.local`, `sibyl`, `localhost`, `<hostname>.local`, the hostname when it is a local name, and every private IPv4 address of the host. Sibyl issues a new one at boot, within seconds of an address or hostname change, and before it expires. A host from before 2026-10-29 had a self-signed certificate. Its first boot with this version replaces it, so trust the new `ca.crt` once.

### Arcane password

A new Arcane install has the login `arcane` / `arcane-admin`. The System API replaces that password with a random one within about 10 seconds of the first start of Arcane. The Status screen of `sibyl` shows the new password. Log in to Arcane at `https://<address>:9443/` with the user `arcane` and that password, and choose your own password. The line on the Status screen then goes away.

## Use Sibyl

| Service | Address | Use |
|---|---|---|
| Open WebUI | `https://<address>/` | Chat in the browser, with document search |
| OpenAI-compatible API | `https://<address>:8443/v1` | Programs and tools that speak the OpenAI API |
| Anthropic-compatible API | `https://<address>:8443/v1/messages` | Programs and tools that speak the Anthropic Messages API |
| Ollama-compatible API | `https://<address>:8443/api` | Programs and tools that speak the Ollama API |
| Arcane | `https://<address>:9443/` | Other containers on the same host |

### The API keys

Sibyl has two kinds of key:

- An inference key belongs to one client of the model router. It works for the OpenAI, Anthropic and Ollama APIs, and nothing else.
- The System API key manages models, accounts, disks and the router. The `sibyl` console uses it. Do not give it to programs. It does not work for inference.

The first boot makes three keys with `openssl rand`, and stores them as Podman secrets of the `sibyl` user:

| Key | Secret | Use |
|---|---|---|
| System API key | `sibyl-api-key` | Copied to `/var/lib/sibyl-operator/api.key` for the console |
| Inference key of the client `default` | `sibyl-inference-key` | Copied to `/var/lib/sibyl-operator/inference.key` |
| Inference key of the client `open-webui` | `sibyl-router-owui-key` | Given to Open WebUI only |

The administrator and operators can read both files in `/var/lib/sibyl-operator`.

Give each program its own key. Make one on the Router screen of `sibyl` (key `6`, then `n`), or with `POST /system/router/clients`. Sibyl stores only the hash of a client key, so it shows the key once. Delete the client to revoke its key. The `default` key is for a quick test:

```bash
cat /var/lib/sibyl-operator/inference.key
```

### Call the OpenAI API

```bash
KEY="$(ssh admin@<address> cat /var/lib/sibyl-operator/inference.key)"
curl --cacert sibyl-ca.crt -H "Authorization: Bearer $KEY" https://<address>:8443/v1/models
```

```bash
curl --cacert sibyl-ca.crt -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  https://<address>:8443/v1/chat/completions \
  -d '{"model":"<model id>","messages":[{"role":"user","content":"Hello"}]}'
```

In a program, set the base URL to `https://<address>:8443/v1` and the API key to its client key. The model ID is a name from `/v1/models`.

The first request to a model loads it into GPU memory. That takes from a few seconds to a few minutes for a large model.

### Call the Ollama API

Set the Ollama host to `https://<address>:8443` and send the key as a bearer token. A client that cannot send an `Authorization` header cannot use Sibyl.

```bash
curl --cacert sibyl-ca.crt -H "Authorization: Bearer $KEY" https://<address>:8443/api/tags
```

Sibyl downloads models itself, so `ollama pull` and the other model management calls return an error that says so.

### Call the Anthropic API

Tools that speak the Anthropic Messages API, such as Claude Code, can use the local models. Set the base URL to `https://<address>:8443` and the API key to a client key. Sibyl reads the key from the `x-api-key` header or from `Authorization: Bearer`.

```bash
curl --cacert sibyl-ca.crt -H "x-api-key: $KEY" -H "Content-Type: application/json" \
  https://<address>:8443/v1/messages \
  -d '{"model":"<model id>","max_tokens":200,"messages":[{"role":"user","content":"Hello"}]}'
```

Sibyl translates the request for the local model. Text, images, tools and streaming work. Thinking blocks are not returned, and server tools, such as web search, are left out of the request.

### Client keys and the model router

Every request to `/v1` goes through the model router. The router checks the key, and then sends the request to a local model, or to a cloud provider when you allow that. With no provider configured, every request stays on the host.

Each key belongs to a client. A client has one of two policies:

- `local-only`: the client's requests never leave the host. This is the default.
- `hybrid`: the routing rules can send the client's requests to a provider.

Two clients exist from the start: `default` and `open-webui`. Both are `local-only`.

The Router screen of the `sibyl` console (key `6`) creates clients, sets their policies and budgets, and adds providers. The web console at `https://<address>:8443/console/` has the same screens in a browser: Router, Models, Downloads, Hardware, Host, Status, and a Setup page with the steps of `sibyl setup`. Sign in with the user and password of a host account in the `wheel` group, such as the admin account from the install. After 5 failed sign-ins from one address in a minute, that address must wait a minute. A session ends after 12 hours, or after 1 hour without use. The System API does the same at `https://<address>:8443/system/router`. These examples use the System API key in `$ADMIN`:

```bash
ADMIN="$(ssh admin@<address> cat /var/lib/sibyl-operator/api.key)"
```

Create a client. The answer holds the new key. Copy it, because Sibyl stores only its hash and cannot show it again:

```bash
curl --cacert sibyl-ca.crt -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  https://<address>:8443/system/router/clients -d '{"name":"laptop","policy":"local-only"}'
```

Add a provider. On the Router screen, press `p` and pick the provider from the models.dev index, which Sibyl keeps on the host and updates daily. Sibyl fills in the API and the base URL, and lists the provider's models with their context size and price, so you can choose the models to list. "Other" takes any OpenAI-compatible URL by hand. Sibyl checks the key with the provider before it stores it.

With the System API, give the fields yourself. The type `openai` covers OpenAI, OpenRouter and other services with an OpenAI-compatible API. The type `anthropic` is the Anthropic API, and needs no base URL:

```bash
curl --cacert sibyl-ca.crt -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  -X PUT https://<address>:8443/system/router/providers/openrouter \
  -d '{"type":"openai","base_url":"https://openrouter.ai/api/v1","api_key":"<provider key>","models":["<model>"]}'
```

Let a client use providers. Do this only for clients whose prompts can leave the host:

```bash
curl --cacert sibyl-ca.crt -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  -X PUT https://<address>:8443/system/router/clients/laptop -d '{"policy":"hybrid"}'
```

A `hybrid` client can then ask for `<provider>/<model>`, such as `openrouter/<model>`, or for `auto`. The model `auto` runs the rules of the `auto` profile, which you set on the Router screen or with `PUT /system/router/profiles`. [router-design.md](router-design.md) lists the rule conditions.

The Router screen has a key for each part of the profiles:

- `m` makes, changes or deletes a profile: its local model, its fallback routes and its local wait.
- `r` adds, edits, moves or deletes a rule. The rule form shows the common conditions. It keeps the conditions that it does not show.
- `c` sets the calibration: the sample rate, the daily budget, the judge, the provider to compare with, the keep time, the measurement of new local models and the automatic proposals.
- `s` sets the sensitive words and the classifier.
- `a` applies a calibration proposal, and `C` deletes every calibration sample. `POST /system/router/explain` shows the route for a request without sending it, and `GET /system/router/stats` shows the requests per client and per route.

Sibyl prices each provider request from the models.dev index, so budgets work for most providers without a price of your own. A price in the provider's `prices` overrides the index. A model that the index does not list for that provider has no price.

A new client has no spending limit. Set a budget on the Router screen, or with `"budget":{"daily_usd":5,"monthly_usd":50}` in the client's `PUT`. A `PUT` without `budget` removes the budget. When a budget is spent, `auto` requests run on the local model, and requests for a provider model get 429. Sibyl knows the Anthropic prices. For any other provider, add `"prices"` to the provider, or a client with a budget cannot use it.

Each response has an `X-Sibyl-Route` header that names the route, such as `local/qwen3-32b-q4_k_m`. A client can send `X-Sibyl-Route: local` to keep one request on the host.

#### Route by task and difficulty

A rule can test two labels of a request: `task` (chat, code, math, writing or extraction) and `difficulty` (easy, medium or hard). The classifier gives the labels on the host, so no prompt leaves it to be labelled. It embeds the last user message with the local embedding model, and the 5 most similar labelled examples vote. Sibyl ships 150 starter examples, 10 for each pair of labels. The classifier runs only for a profile that has a `task` or `difficulty` rule. When it fails or takes more than 300 ms, both labels are `unknown`, and the next rules decide.

This rule sends hard code and maths to a provider and keeps the rest local:

```yaml
profiles:
  auto:
    local_model: <local model>
    rules:
      - name: secrets stay on the host
        when: {sensitive: true}
        route: local
      - name: hard code and maths
        when: {task: [code, math], difficulty: [hard]}
        route: <provider>/<model>
      - name: everything else
        route: local
    fallback: [local]
```

To see the labels of a text and its nearest examples, send it to the classifier:

```bash
curl --cacert sibyl-ca.crt -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  https://<address>:8443/system/router/classify -d '{"text":"Find the race in this Go code"}'
```

If the classifier labels your own requests wrongly, add examples of them. `PUT /system/router/examples` takes a JSON list of `{"text", "task", "difficulty"}` and replaces your list. Your examples add to the starter examples. To use only yours, set `classifier: {no_starter: true}` in the profiles. Other classifier settings are `model` (the embedding model, by default the first one installed), `k` (the number of voting examples, default 5), `timeout_ms` (default 300) and `prefix`. The prefix goes before every text that the classifier embeds. The default is the instruction format of the Qwen3 embedding models. For another embedding model, set the prefix that its model card gives, or `""` for none.

To compare profiles before you trust one, run `sibyl-eval` from a clone of the Sibyl repository. It sends a labelled request set to the classifier and to explain, so no request leaves the box. It prints the classifier's accuracy and, for each profile, the share of local requests and the estimated provider cost:

```bash
go run ./cmd/sibyl-eval -url https://<address>:8443 -api-key api.key -ca sibyl-ca.crt -profiles auto,<other profile>
```

The default set is `internal/router/heldout.jsonl`: 60 requests, 4 for each pair of labels, none of them in the starter examples. `-send -key inference.key -out answers.jsonl` also sends each request and keeps the answers for you to read. Provider requests then cost money. `-judge <model>` adds a score from 1 to 10 for each answer from that model.

### Calibration

Calibration measures the local model against a provider on your own requests. Turn it on per client with `"calibrate": true` in `PUT /system/router/clients/{name}`, or with the Calibrate field on the TUI Router screen. The router then keeps 2% of that client's chat requests to a profile, with the answers, in `/var/lib/sibyl/config/router/calibration/`. These samples are the only message content that Sibyl keeps. The router never keeps a request that is sensitive, that has images or tools, or that comes from a `local-only` client.

When no local request is in flight, the router gets the answer of the other side for each sample. A local answer gets an answer from the profile's first provider route, and a provider answer gets one from the local model. The router does not load a local model for this. A judge then compares the two answers in a random order, without the model names. The judge is the provider model with the highest output price.

Calibration has its own provider budget, 1 US dollar per UTC day by default. A provider model with no price cannot be counted against it, so calibration waits until you add a price for it in the provider's `prices`. Set the calibration in the profiles:

```yaml
calibration:
  sample_rate: 0.02     # share of requests kept
  daily_usd: 1          # 0 stops all provider calls
  judge: openrouter/<model>   # default: the provider model with the highest output price
  compare: openrouter/<model> # default: the profile's first provider route
  keep_days: 14
  max_samples: 2000
```

`GET /system/router/calibration` reports, per local model, task and difficulty, how often the local answer won or tied, with the provider cost and the latency of each side. `DELETE /system/router/calibration` deletes every sample.

After 50 comparisons for a task and difficulty, the report proposes a rule change when the results disagree with the profile. A request that the profile sends to a provider is proposed for the local model when the local answer won or tied at least 90% of the time. A request that the profile keeps local is proposed for the provider when the local answer won or tied less than 60% of the time. To apply a proposal, send its `id`:

```bash
curl --cacert sibyl-ca.crt -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  https://<address>:8443/system/router/calibration/apply -d '{"id":"<id>"}'
```

When a profile gets a local model that has not been measured, the router measures it first with the 60 labelled requests of `sibyl-eval`. The router answers them with the local model when it is idle, and gets the provider answers and the verdicts under the same daily budget. Set `no_seed: true` in `calibration` to turn this off.

To let Sibyl apply proposals by itself, set `auto_apply: true` in `calibration`. Sibyl then applies a proposal only when the profile already names its route, and it logs each change.

A rule can also test the calibration results of each request directly. `needs_provider: true` matches a request when most of the nearest judged requests went better at the provider. Below 20 judged requests for the local model, the condition matches nothing.

### The sibyl console

Run `sibyl` on the host, at the console or over SSH. It is a terminal interface with six screens. Press a number or Tab to change screens, and `q` to quit.

| Key | Screen | What you can do |
|---|---|---|
| `1` | Status | See the addresses, GPU memory and the service states. `r` restarts a service. `o` connects Open WebUI. |
| `2` | Hardware | `e` changes the GPUs, the backend and the default layout. |
| `3` | Models | `enter` edits a model's settings. `b` runs the benchmark. `d` deletes the model. |
| `4` | Download | `/` searches Hugging Face. `t` sets a Hugging Face token. `c` cancels a download, `R` retries one. |
| `5` | Host | `h` hostname, `n` network, `a` add operator, `k` keys, `d` delete operator, `p` new model pool, `x` detach pool. |
| `6` | Router | `n` new client and key, `k` new key for a client, `e` edit its policy and budget, `d` delete a client, `p` add or change a provider, `x` delete a provider. It shows the spend per client and the last decisions. |

The bottom line of each screen lists its keys.

## Models

Sibyl runs GGUF models with llama.cpp. llama-swap loads a model when a request asks for it, and unloads models when the GPUs need room for another.

### Download a model

1. Open the Download screen (`4`) and press `/`.
2. Type a search, such as `qwen3 gguf`, and press Enter. You can also paste a full name, such as `Qwen/Qwen3-Embedding-0.6B-GGUF`, to open that repository.
3. Choose a repository, then a variant. A variant is one quantization of the model, such as `Q4_K_M` or `Q8_0`. Smaller quantizations need less memory and lose some quality.
4. The fit column tells you how the variant suits your GPUs:

| Fit | Meaning |
|---|---|
| `gpu` | The file fits in the VRAM of the selected GPUs, with room for the context. |
| `split` | The model needs system RAM as well, and it runs slower. |
| `no` | The model is too large for this host. |
| `?` | The GPU does not report its VRAM. This is normal on NVIDIA and Intel. |

5. If the host has model pools, choose where the model goes. Then confirm. The download runs in the background, and you can leave the screen. Sibyl checks each file against the checksum from Hugging Face, and the model appears on the Models screen when it is complete.

A download that stops resumes where it stopped. Press `R` to retry a failed download.

### Gated and private models

Some models need a Hugging Face account. Accept the model's terms on huggingface.co first. Then create an access token with read access at [huggingface.co/settings/tokens](https://huggingface.co/settings/tokens), press `t` on the Download screen, and paste it. Sibyl checks the token with Hugging Face and shows the account name.

### Model settings

On the Models screen, select a model and press Enter. The settings apply as soon as you save them.

| Setting | Effect |
|---|---|
| Layout | Span across all selected GPUs, or pin to one GPU. |
| When VRAM is short | What an automatic context gives up. Quality (the default) keeps the KV cache at f16 and shortens the context. Context quantizes the KV cache only as far as it must, to `q8_0` and then `q4_0`, to keep the native context. |
| Context size | The longest conversation in tokens. Empty lets Sibyl choose: the native context if it fits on the model's GPUs, else the largest multiple of 4,096 tokens that fits in 92% of their VRAM, less the memory of the embedding models on them. An embedding model with no context size gets 8,192 tokens. The field shows what Sibyl chooses and its memory estimate. A number that you enter is kept even when it does not fit, and the field then says that llama.cpp keeps part of the model in system RAM, which is slower. |
| Embedding | Marks a model for embeddings. Open WebUI uses the first embedding model for document search. |
| Hidden | Leaves the model out of the model lists. |
| Preset | A sampling preset for the model's main use: `general`, `coding`, `agentic` or `writing`. See [Sampling presets](#sampling-presets). |
| Sampling | Default temperature, top-k, top-p, min-p and penalties. An empty field shows in grey the value that applies: the preset's, the model file's recommendation, or llama.cpp's default. A value that you enter wins over the preset. A client can still override them per request. |
| Reasoning effort | `low`, `medium` or `high`, for a model whose chat template reads `reasoning_effort`, such as gpt-oss and some Qwen models. Sibyl passes it to llama-server as `--chat-template-kwargs`. Empty leaves the template's default. A request's own `reasoning_effort` wins. A template that does not read it ignores it. |
| Parallel requests | The number of requests that the model answers at once. The requests share the context, so each one is sure of the context size divided by this number. Empty leaves llama.cpp's default. More slots serve more programs at the same time, and each one gets a shorter context. |
| Tensor split | For a model that spans GPUs, the share of its layers on each GPU, such as `3,2`. Empty lets Sibyl split by the memory that each GPU has left after the embedding models. |
| KV cache | The precision of the context memory. `q8_0` halves its size with little loss. A type that you choose here is kept, and an automatic context fits around it. |
| Speculative decoding | Speeds up generation with a small draft model or with n-gram lookup. |
| Experts in system RAM | Mixture of Experts models only. Keeps the expert weights of the first N layers, or `all` of them, in system RAM. The GPUs then hold the rest of the model and a longer context, and the CPU computes the experts. A model larger than the RAM still runs: the kernel reads unused experts back from disk when a token needs them, which is slow. |
| Expert GPU cache | Mixture of Experts models only, with experts in system RAM. GPU memory, in MiB, that keeps the most used experts so that the GPUs compute them. The automatic context counts the cache, so a larger cache gives a shorter context. |

### Sampling presets

A preset starts from the model's recommended sampling: the values in the model file (`general.sampling`), else the model family's published values (Qwen, Gemma, Llama, DeepSeek, Mistral, gpt-oss), else llama.cpp's defaults. It then adjusts them for a use:

| Preset | Change |
|---|---|
| `general` | None: the recommendation as it is. |
| `coding` | Temperature at most 0.6, so code is more consistent. |
| `agentic` | Temperature at most 0.6, so tool calls are more consistent. |
| `writing` | Temperature at least 0.8, and min-p at least 0.05, which keeps a higher temperature coherent. |

gpt-oss keeps OpenAI's one setting, temperature 1.0 and top-p 1.0, for every preset. These adjustments are Sibyl's own defaults, not the model vendors'.

A client can pick a preset for one request without a model reload: ask for `<model>:<preset>`, such as `qwen3.8-flash-next-reap-288-fn-r1:coding`. The model lists show each model with each preset. The preset fills only the fields that the request leaves out.

### Benchmark the backends

Select a model on the Models screen and press `b`. Sibyl unloads the running models and measures each backend of your variant on that model. It takes a few minutes, and the result shows on the Models screen with a recommendation. To use the faster backend, change it on the Hardware screen.

### Model pools

A model pool is an extra disk for models. Sibyl encrypts it and mounts it under `/var/lib/sibyl-pools`.

1. On the Host screen, press `p` and choose the disk. The disk is erased.
2. Type the disk's serial number to confirm.
3. Sibyl shows the pool's recovery key once. Write it down. You need it to move the pool to another computer.

Downloads can then go to the pool. To remove a pool, press `x`. The data stays on the disk.

## Host settings and accounts

### Operators

An operator can log in over SSH and run `sibyl`, but cannot use `sudo`. Add operators on the Host screen with `a`, with SSH keys or a GitHub user name. An operator can read the System API key, so add only people whom you trust with the whole server.

To let an existing account run `sibyl`, an administrator runs:

```bash
sudo usermod -aG sibyl-operators <user>
```

The user must log out and back in.

### Network

On the Host screen, press `n` to choose DHCP or a static IPv4 address. The change rolls back after 120 seconds unless you press `c`. If you connect over SSH, reconnect at the new address within that time and press `c` there.

The TLS certificate keeps the address that it was made with. After an address change, use the name `sibyl.local` or trust the warning again.

## Run other containers with Arcane

Arcane at `https://<address>:9443/` runs other containers and Compose stacks beside Sibyl. It uses the same Podman as the Sibyl services, so it can also stop them. A stopped Sibyl service starts again at the next boot.

The host accepts images from the common public registries: Docker Hub, Quay, GitHub, Google, Microsoft, Amazon ECR Public, NVIDIA, GitLab, Chainguard and LinuxServer. An administrator can allow another registry in `/etc/containers/policy.json`.

## Updates

Sibyl does not update by itself. The operating system and the services update together, as one image.

### Online update

On the Status page of the web console, press "Check for an update". When a version is available, press "Apply update". With "Reboot into it when it is staged" on, the host reboots by itself when the download ends. Without it, the page offers "Reboot now" when the update is staged. In the TUI, the Status screen has the same controls: `u` checks, `U` applies, and `B` reboots into a staged update.

From a shell, the same steps are:

```bash
sudo bootc upgrade
sudo systemctl reboot
```

`bootc upgrade` downloads the new image and checks its signature. The new version starts at the next boot. `sudo bootc status` shows the running version and the one that is staged.

### Roll back

If a new version has a problem, go back to the previous one:

```bash
sudo bootc rollback
sudo systemctl reboot
```

Your models, settings and accounts are in `/var`, which an update and a rollback do not change.

### Offline update

A host with no internet updates from a bundle on a USB drive.

1. On a computer with internet, download the bundle for your variant to a USB drive of 8 GB or more:

```bash
oras pull ghcr.io/diffsec/sibyl-bundle:latest-amd -o /path/to/usb/sibyl-bundle
```

2. Plug the drive into the Sibyl host and mount it:

```bash
lsblk
sudo mount /dev/sdX1 /mnt
```

3. Apply the bundle and reboot:

```bash
sudo sibyl-update apply /mnt/sibyl-bundle
sudo systemctl reboot
```

`sibyl-update` checks the bundle's signature and every file before it changes anything. After an offline update, the host follows the bundle. When it has internet again, return to online updates with the command that `sibyl-update` prints.

## Disk encryption and recovery

The root disk is encrypted with LUKS2. The TPM unlocks it at boot while the Secure Boot settings stay the same.

### When the TPM does not unlock the disk

A firmware update, a change to Secure Boot, or new Secure Boot keys can stop the TPM from unlocking the disk. The boot then asks for a passphrase. Type the recovery key, with its dashes.

After the boot, bind the disk to the TPM again so the next boot unlocks by itself. Find the encrypted partition with `lsblk`, then:

```bash
sudo systemd-cryptenroll --wipe-slot=tpm2 --tpm2-device=auto --tpm2-pcrs=7 /dev/nvme0n1p3
```

The command asks for the recovery key. Replace `/dev/nvme0n1p3` with your partition.

### Check the encryption

```bash
sudo systemd-cryptenroll /dev/nvme0n1p3
```

The list must show a `tpm2` slot and a `recovery` slot.

## Try Sibyl from the USB without an install

The live USB runs Sibyl without changing the internal disks. The services need a persistence partition on the stick, because their images do not fit in memory.

1. Use a USB stick of 32 GB or more.
2. Boot from it, and choose `2` in the menu. Sibyl adds a partition in the free space of the stick.
3. Reboot from the stick. The services start from the persistence partition.
4. Choose `3` in the menu to run the setup wizard.

The live system logs in as root at the console without a password, and the persistence partition is not encrypted. Use it only to try Sibyl, on a network that you trust.

## Troubleshooting

### A service does not start

The Status screen in `sibyl` shows each service. From a shell, as the administrator:

```bash
sudo systemctl --user -M sibyl@ status llama-swap
sudo journalctl _SYSTEMD_USER_UNIT=llama-swap.service -n 50
```

The services are `llama-swap`, `open-webui`, `system-api`, `caddy` and `arcane`. To restart one, select it on the Status screen and press `r`.

### A model does not load

1. Look at the llama-swap log with the command above. An "out of memory" error means that the model, or its context size, is too large for the selected GPUs. Choose a smaller quantization or a smaller context size.
2. On AMD, if the HIP backend fails on a card that Sibyl does not recognize, change the backend to Vulkan on the Hardware screen. Sibyl refuses HIP for the cards that it knows the HIP image does not support.

### `sibyl` says that you are not in the sibyl-operators group

An administrator adds you with `sudo usermod -aG sibyl-operators <user>`. Log out and back in.

### The browser cannot reach Sibyl

1. Make sure that you use `https://`, and the right port: no port for Open WebUI, 8443 for the APIs, 9443 for Arcane.
2. The host firewall allows only these ports and SSH. Make sure that no firewall between your computer and the host blocks them.

### Report a problem

Open an issue at [github.com/diffsec/sibyl/issues](https://github.com/diffsec/sibyl/issues). Include the output of `sudo bootc status`, the Status screen, and the log of the service that fails. Do not include a key.
