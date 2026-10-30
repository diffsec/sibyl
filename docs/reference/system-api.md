# System API reference

The System API is the management API of a Sibyl host. The `sibyl` TUI, the web console and the setup wizard use it, and you can call it from your own scripts. This page lists every route. The code is in `cmd/sibyl-api` and `internal/api`, and the routes are registered in `Handler()` in `internal/api/api.go`.

The design of the System API, the host agent and `sibyl-hostd` is in [the design overview](../design/overview.md). The router's own design is in [the router design](../design/router.md).

## Contents

1. [Base URL and authentication](#base-url-and-authentication)
2. [Requests, responses and errors](#requests-responses-and-errors)
3. [Host actions and the 202 queued response](#host-actions-and-the-202-queued-response)
4. [Status and services](#status-and-services)
5. [Hardware](#hardware)
6. [Models](#models)
7. [Downloads and Hugging Face](#downloads-and-hugging-face)
8. [Host and pools](#host-and-pools)
9. [Updates](#updates)
10. [Router](#router)
11. [Calibration](#calibration)
12. [Integrations](#integrations)
13. [Other endpoints on port 8443](#other-endpoints-on-port-8443)

## Base URL and authentication

Send every request to this base URL:

```
https://<address>:8443/system
```

Caddy, the TLS proxy of the host, removes the `/system` prefix and sends the request to the System API container. For example, `GET https://<address>:8443/system/status` reaches the route `GET /status`.

Every route except `GET /health` needs the System API key as a bearer token:

```bash
curl --cacert ca.crt -H "Authorization: Bearer $(cat api.key)" https://<address>:8443/system/status
```

- The key is in `/var/lib/sibyl-operator/api.key` on the host. Members of the `sibyl-operators` group can read it.
- The CA certificate of the host is in `/var/lib/sibyl-operator/ca.crt`. [First boot](../getting-started/first-boot.md) explains how to trust it.
- A missing or wrong key returns `401` with `{"error": "missing or invalid API key"}`.
- The System API key gives full control of the host. It is not an inference key. The router refuses it on `/v1`, and the Ollama shim refuses it on `/api`. [API clients](../guides/api-clients.md) explains the inference keys.

The web console is also served by the System API, under `/console/`. Its pages are not API routes. They use a session cookie, not the API key. [Consoles](../guides/consoles.md) describes it.

## Requests, responses and errors

Request and response bodies are JSON.

- The System API refuses a body with an unknown field, and a body over 1 MiB. Both return `400` with `invalid JSON body: ...`.
- A route that takes a body needs a body, even when every field is optional. Send `{}` in that case.
- An error is a JSON object with one field: `{"error": "<message>"}`. The message says what to change.

| Status | Meaning |
|---|---|
| `200` | Done. The body holds the result. |
| `201` | Created. Only `POST /router/clients` returns it. |
| `202` | Accepted. The work continues in the background. See [the next section](#host-actions-and-the-202-queued-response). |
| `400` | The body is not valid JSON, has an unknown field, or a query parameter is wrong. |
| `401` | The API key is missing or wrong. |
| `404` | The model, download, client, provider, proposal or benchmark does not exist. |
| `409` | The request conflicts with the current state, such as a built-in client or a download that cannot be canceled. |
| `422` | A value failed a check. The host was not changed. |
| `500` | The System API failed to read or write its files. |
| `502` | An upstream service failed: Hugging Face, a provider, Open WebUI, the router or models.dev. |
| `503` | A part is not configured or not ready, such as `sibyl-hostd` before it writes its state. |

## Host actions and the 202 queued response

Some routes change the host itself: the hostname, the network, accounts, pools, updates and reboots. These changes need root. The System API runs as the unprivileged `sibyl` user, so it queues a request for `sibyl-hostd`, the root agent of the host.

1. The System API checks the request with the same rules as `sibyl-hostd`. A request that fails the check returns `422` and is not queued.
2. The System API writes the request to the queue and waits up to 45 seconds for the result.
3. If the result arrives in time, the route returns `200` with the result. If the action failed, the route returns `422` with the result.
4. If the result does not arrive in time, the route returns `202` with `{"id": "<request id>", "status": "queued"}`. Call `GET /host/requests/{id}` later to read the result.

A host action result has these fields:

| Field | Content |
|---|---|
| `id` | The request ID |
| `action` | The action, such as `hostname`, `network` or `pool-create` |
| `ok` | `true` if the action succeeded |
| `error` | The reason for a failure |
| `message` | Extra information for the user |
| `secret` | A value that is shown once and never logged |
| `finished` | The time the action finished |

If host actions are not configured, these routes return `503` with `host actions are not configured`.

Service restarts and benchmarks use a different agent, the host agent. These routes return `202` at once and do not wait. `POST /services/{name}/restart` returns `{"status": "queued"}`. `POST /models/{id}/benchmark` returns `{"status": "queued"}`, and `GET /models/{id}/benchmark` shows the progress.

## Status and services

| Method and path | Body | Response | What it does |
|---|---|---|---|
| `GET /health` | None | `{"status": "ok"}` | Liveness check. Needs no API key. |
| `GET /status` | None | `version`, `image_tag`, `variant`, `backends`, `gpus` (count), `hardware`, `services`, and `errors` if a part failed | The overview that the Status screens show. |
| `GET /services` | None | `updated` and `units`: a map of unit name to `active`, `sub` and `since` | The states of the Sibyl units. The host writes them every 15 seconds. |
| `POST /services/{name}/restart` | None | `202`, `{"status": "queued"}` | Asks the host agent to restart one unit. `{name}` is one of `llama-swap`, `router`, `open-webui`, `caddy`, `arcane` or `system-api`. Another name returns `400`. |
| `POST /apply` | None | The apply result (see below) | Regenerates the llama-swap configuration and the backend drop-in from the current files. |

An apply returns this result. `PUT /hardware`, `PUT /models/{id}`, `DELETE /models/{id}` and `POST /apply` return it.

| Field | Content |
|---|---|
| `models` | The number of models in the llama-swap configuration |
| `issues` | Models with a problem, and whether they were skipped |
| `config_changed` | `true` if the llama-swap configuration changed. llama-swap reloads it by itself. |
| `backend_changed` | `true` if the backend or the GPU set changed. The host agent then reloads systemd and restarts `llama-swap`. |

After each apply, the System API also updates Open WebUI in the background. See [Integrations](#integrations).

## Hardware

| Method and path | Body | Response | What it does |
|---|---|---|---|
| `GET /gpus` | None | A list of GPUs in PCI order | The GPUs that the System API detects in sysfs. |
| `GET /hardware` | None | `backend`, `gpus`, `layout` | The current hardware choice. |
| `PUT /hardware` | `{"backend", "gpus", "layout"}` | The apply result | Saves the hardware choice and applies it. |

Each GPU in `GET /gpus` has these fields: `index`, `pci_address`, `vendor`, `vendor_id`, `device_id`, `driver`, `vram_total_bytes`, `vram_used_bytes`, `pcie_link_width`, `pcie_link_speed` and `arch`. Some fields are missing when sysfs does not report them.

The hardware fields:

| Field | Values |
|---|---|
| `backend` | `hip`, `vulkan` or `cuda`. The host image variant decides which backends exist. `GET /status` lists them in `backends`. |
| `gpus` | The selected GPUs, as indexes into the `GET /gpus` list |
| `layout` | `span` or `pin`: the default way to place a model on the selected GPUs |

A value that does not fit the variant or the detected GPUs returns `422`.

## Models

| Method and path | Body | Response | What it does |
|---|---|---|---|
| `GET /models` | None | A list of installed models | Every model in the models directory and in the model pools, with its settings. |
| `PUT /models/{id}` | The model settings | The apply result | Replaces the settings of one model and applies them. |
| `DELETE /models/{id}` | None | The apply result | Deletes the model file and all of its shards, removes its settings, and applies. |
| `POST /models/{id}/benchmark` | None | `202`, `{"status": "queued"}` | Asks the host agent to run `llama-bench` on the model with each backend image. |
| `GET /models/{id}/benchmark` | None | The benchmark state | The progress or the result of the last benchmark. `404` if no benchmark ran. |

Each model in `GET /models` has these fields:

| Field | Content |
|---|---|
| `id`, `path`, `pool`, `size_bytes`, `shards` | The model, its file, its pool (empty for the models directory), its size and its number of shard files |
| `settings` | The settings that the owner chose |
| `fit` | The context size and KV cache types that the model runs with, where Sibyl chooses them |
| `meta` | What the model file says about its context. Missing if the file header cannot be read. |
| `benchmark` | The last benchmark, if one ran |

The settings body of `PUT /models/{id}` has these fields: `layout`, `ctx_size`, `embedding`, `hidden`, `sampling`, `speculative`, `kv_cache`, `moe`, `fit`, `preset`, `reasoning`, `parallel`, `tensor_split` and `args`. [Configuration](configuration.md) describes each field and its range. The route returns `404` for a model that is not installed. It returns `422` in these cases:

- A value is out of range.
- `moe` is set, but the model file has no experts.
- `reasoning` does not match what the model's chat template reads.
- `speculative.draft_model` names the model itself, or a model that is not installed.
- An extra argument in `args` does not match `^[A-Za-z0-9_.,:=/+-]+$`, or sets a flag that Sibyl sets, such as `--model`, `--port` or `--device`.

The benchmark state has these fields:

| Field | Content |
|---|---|
| `status` | `queued`, `running`, `done` or `failed` |
| `updated` | The time of the last status change |
| `results` | Per backend: `prompt_tokens_per_second`, `generation_tokens_per_second`, or `error` |
| `recommended` | The backend with the fastest token generation |

The benchmark leaves out HIP when a selected GPU has a ROCm target that the HIP image does not support. That backend then shows the error `not run: the HIP image does not support a selected GPU`.

## Downloads and Hugging Face

| Method and path | Body | Response | What it does |
|---|---|---|---|
| `GET /hf/search?q=<term>&limit=<n>` | None | A list of repositories | Searches Hugging Face for GGUF repositories, most downloaded first. `q` must be 1 to 200 characters. `limit` is 1 to 100, and the default is 30. |
| `GET /hf/repos/{owner}/{name}?revision=<sha>` | None | `repo`, `revision`, `vram_bytes`, `ram_bytes`, `variants` | The complete GGUF variants of a repository at one commit, each with its size and `fit`. Without `revision`, the System API uses the current commit. A given `revision` must be a 40-character commit SHA. |
| `GET /downloads` | None | A list of download jobs | Every download job. |
| `POST /downloads` | `{"repo", "variant", "revision", "pool"}` | `202` and the new job | Queues a download. `revision` and `pool` are optional. Without `pool`, the model goes to the models directory. |
| `GET /downloads/{id}` | None | One job | The progress of one job. `404` if it does not exist. |
| `DELETE /downloads/{id}` | None | `{"status": "canceled"}` | Cancels a job. `409` if the job cannot be canceled. |
| `POST /downloads/{id}/retry` | None | `202`, `{"status": "queued"}` | Resumes a failed job. `409` if the job cannot be retried. |

The `fit` of a variant is one of these values:

| Value | Meaning |
|---|---|
| `gpu` | The file fits in the VRAM of the selected GPUs, less 2 GiB per GPU for the KV cache. |
| `split` | The model needs system RAM as well. |
| `no` | The model is larger than the VRAM plus half of the system RAM. |
| `unknown` | The System API does not know the VRAM of a selected GPU. |

A download job has these fields: `id`, `repo`, `revision`, `variant`, `dir`, `pool`, `files`, `status`, `bytes_done`, `bytes_total`, `error`, `created` and `updated`. `status` is `queued`, `downloading`, `verifying`, `done`, `failed` or `canceled`. A job that fails a check returns `422` from `POST /downloads`. When a job finishes, the System API applies the configuration.

The Hugging Face token routes are under [Integrations](#integrations). [Models](../guides/models.md) explains downloads and gated models.

## Host and pools

All routes in this section except `GET /host` and `GET /pools` are host actions. They return `200`, `202` or `422` as [host actions](#host-actions-and-the-202-queued-response) describes.

| Method and path | Body | Response | What it does |
|---|---|---|---|
| `GET /host` | None | The host state (see below) | What `sibyl-hostd` last saw on the host. `503` before `sibyl-hostd` writes its first state. |
| `GET /host/requests/{id}` | None | A host action result, or `202` with `{"id", "status": "queued"}` | The result of a host action that took longer than 45 seconds. |
| `PUT /host/hostname` | `{"hostname"}` | A host action result | Sets the static hostname. |
| `PUT /host/network/{iface}` | `{"method", "address", "gateway", "dns", "rollback_seconds"}` | A host action result | Sets DHCP or a static IPv4 address on one ethernet interface. The change rolls back by itself unless a confirm follows. |
| `POST /host/network/confirm` | None | A host action result | Keeps the last network change. |
| `PUT /host/users/{name}` | `{"keys", "github"}` | A host action result | Creates an operator account, or replaces its SSH keys. |
| `DELETE /host/users/{name}` | None | A host action result | Deletes an operator account that `sibyl-hostd` created, and its home directory. |
| `GET /pools` | None | `{"pools", "disks"}` | The model pools, and the disks that can become a pool. |
| `POST /pools` | `{"disk", "confirm"}` | A host action result | Erases a disk and makes it a model pool. |
| `DELETE /pools/{name}` | None | A host action result | Unmounts a pool and keeps its data. |

The host state of `GET /host` has these fields:

| Field | Content |
|---|---|
| `updated` | The time `sibyl-hostd` wrote the state |
| `hostname` | The hostname |
| `interfaces` | Per interface: `name`, `type`, `mac`, `state`, `connection`, `method`, `addresses`, `gateway`, `dns` |
| `users` | Per account: `name`, `uid`, `groups`, `managed`, `operator`, `admin`, `keys` (the number of SSH keys) |
| `pending_network` | A network change that waits for a confirm: `interface`, `checkpoint`, `expires` |
| `pools` | The model pools |
| `update` | The host image versions. See [Updates](#updates). |
| `disks` | The disks that can become a pool |
| `errors` | Parts of the state that `sibyl-hostd` failed to read |

The fields of the network body:

| Field | Content |
|---|---|
| `method` | `dhcp` or `static` |
| `address` | The IPv4 address with its prefix length, such as `192.168.1.26/24`. Only for `static`. |
| `gateway` | The IPv4 gateway. Only for `static`. |
| `dns` | A list of DNS servers. Only for `static`. |
| `rollback_seconds` | The time to wait for a confirm before the old configuration comes back: 30 to 600. The default is 120. |

After a network change, connect at the new address and call `POST /host/network/confirm` before the time runs out.

The fields of the user body:

| Field | Content |
|---|---|
| `keys` | SSH public keys. The key types are `ssh-ed25519`, ECDSA, security-key types, or RSA of 2048 bits or more. Key options such as `command=` are refused. |
| `github` | Optional. A GitHub user name. The System API adds the keys from `https://github.com/<user>.keys`. If it cannot read them, the route returns `422`. |

`sibyl-hostd` adds accounts only to the `sibyl-operators` group, never to `wheel`. It refuses `root`, `admin`, `sibyl` and other system names.

The fields of the pool body:

| Field | Content |
|---|---|
| `disk` | A whole disk, such as `/dev/sdb` or `/dev/nvme1n1` |
| `confirm` | The serial number of the disk, or `erase` if the disk has none |

`{name}` of `DELETE /pools/{name}` has the form `pool-1`. The System API first asks llama-swap to unload its models, because a loaded model keeps its file open and the pool cannot be unmounted.

[Host](../guides/host.md) explains operators, the network and pools.

## Updates

These routes are host actions. They return `200`, `202` or `422` as [host actions](#host-actions-and-the-202-queued-response) describes.

| Method and path | Body | Response | What it does |
|---|---|---|---|
| `POST /host/update/check` | None | A host action result | Asks the registry for a newer host image. The result is in `update.available` of `GET /host`. |
| `POST /host/update/apply` | `{"reboot": false}` or `{"reboot": true}` | A host action result | Downloads and stages the newer host image. With `"reboot": true`, the host then reboots into it. The body is required. Send `{}` to stage without a reboot. |
| `POST /host/reboot` | None | A host action result | Reboots the host after 5 seconds, for example into a staged update. |

`update` in `GET /host` has these fields: `image`, `booted`, `booted_digest`, `staged` and `available`. [Updates](../guides/updates.md) explains the update and the rollback.

## Router

These routes manage the model router: its clients, providers and profiles. The System API writes the router's files in `/var/lib/sibyl/config/router/`, and the router reloads them within two seconds. [Router](../guides/router.md) explains how to use them, and [Configuration](configuration.md) lists the fields of each file.

| Method and path | Body | Response | What it does |
|---|---|---|---|
| `GET /router` | None | `{"clients", "providers", "profiles"}` | The router configuration. Providers show `key_set` and never the key. |
| `POST /router/clients` | `{"name", "policy", "allow_hint", "budget", "calibrate"}` | `201`, `{"name", "policy", "key"}` | Creates a client. The key is shown only in this response. The router stores only its hash. |
| `PUT /router/clients/{name}` | `{"policy", "allow_hint", "budget", "calibrate"}` | The client list | Changes the policy, hint permission, budget and calibration of a client. This also works for a built-in client. |
| `POST /router/clients/{name}/key` | None | `{"name", "policy", "key"}` | Gives a client a new key, shown once. The old key stops working within two seconds. |
| `DELETE /router/clients/{name}` | None | The client list | Deletes a client. |
| `PUT /router/providers/{name}` | The provider fields and `api_key` | The router configuration | Adds or changes a provider. |
| `DELETE /router/providers/{name}` | None | The router configuration | Deletes a provider and its key. |
| `PUT /router/profiles` | The profiles file as JSON | The router configuration | Replaces the routing profiles after the router's own checks. |
| `POST /router/explain` | `{"client", "format", "hint", "request"}` | The routing decision | Shows the route for a request without sending it. |
| `POST /router/classify` | `{"text"}` | `{"index", "classification"}` | The classifier's task and difficulty labels for a text, with the nearest examples, and the state of the example index. |
| `GET /router/examples` | None | `{"examples", "starter"}` | The owner's classifier examples, and the number of starter examples. |
| `PUT /router/examples` | A list of `{"text", "task", "difficulty"}` | `{"examples", "starter"}` | Replaces the owner's classifier examples. |
| `GET /router/stats` | None | The statistics (see below) | Totals from the decision log since it last rotated, the spend per client, and the last 50 decisions. |
| `GET /router/catalog` | None | `fetched`, `source`, `enabled`, `error`, `providers` | The local copy of the models.dev index: the providers and their number of models. |
| `GET /router/catalog/{id}?q=<text>` | None | `id`, `name`, `type`, `base_url`, `doc`, `env`, `models` | One provider of the index, with its models, prices and limits. `q` filters the models by ID or name. |
| `POST /router/catalog/refresh` | None | The same as `GET /router/catalog` | Fetches the models.dev index now. `503` if the index is turned off, `502` if the fetch fails. |

Rules for clients:

- `policy` is `local-only` or `hybrid`. A new client without a policy is `local-only`. A `local-only` client never reaches a provider.
- `budget` has `daily_usd` and `monthly_usd`.
- `calibrate: true` lets the router keep samples of the client's requests. See [Calibration](#calibration).
- The built-in clients are `default` and `open-webui`. `POST /router/clients` and `DELETE /router/clients/{name}` return `409` for them. Their keys are Podman secrets on the host, so `POST /router/clients/{name}/key` also returns `409`.
- A name that exists returns `409`. A client that does not exist returns `404`. A value that fails a check returns `422`.

Rules for providers:

- The provider fields are `type`, `base_url`, `models`, `prices` and `catalog`.
- A new provider needs `api_key`. A change without `api_key` keeps the stored key.
- The System API checks the key with the provider before it stores it. A refused key returns `422`.
- Keys are stored in files with mode `0600`, never in the configuration file.
- `DELETE /router/providers/{name}` returns `409` while a profile still names the provider. Change the profiles first.

The body of `POST /router/explain`:

| Field | Content |
|---|---|
| `client` | The name of the client whose policy applies |
| `format` | `chat` (the default) or `messages` |
| `hint` | The `X-Sibyl-Route` header value to test with |
| `request` | The request body, in the OpenAI chat or the Anthropic Messages form |

The decision has these fields: `requested`, `profile`, `rule`, `route`, `fallback`, `features`, `rules` (every rule, with match or no match), `classification`, `price` (for a provider route) and `preset`.

The statistics of `GET /router/stats`:

| Field | Content |
|---|---|
| `requests` | The number of requests |
| `by_client`, `by_route` | Requests per client and per route |
| `errors` | Requests that failed |
| `unpriced` | Provider requests whose cost is not known |
| `regenerated` | Per route, the answers that the client asked for again within a minute |
| `spend` | Per client, `today_usd` and `month_usd`, in UTC |
| `last` | The last 50 decisions, newest first |

## Calibration

Calibration compares sampled answers of local models with provider answers, and proposes routing rules. [Router](../guides/router.md) explains it.

| Method and path | Body | Response | What it does |
|---|---|---|---|
| `GET /router/calibration` | None | The report (see below) | The local win-or-tie rate per local model, task and difficulty, from the judged samples, with the rule proposals. |
| `DELETE /router/calibration` | None | `{"deleted": <n>}` | Deletes every calibration sample. The samples are the only message content that Sibyl keeps. |
| `POST /router/calibration/apply` | `{"id"}` | The applied proposal | Adds the rule of one proposal to its profile. `404` if the proposal no longer exists. Read `GET /router/calibration` again in that case. |

The report has these fields:

| Field | Content |
|---|---|
| `samples`, `pending`, `judged`, `failed` | The number of samples: all, waiting for an answer or a verdict, judged, and failed three times |
| `oldest` | The time of the oldest sample |
| `cells` | The results per local model, task and difficulty |
| `status` | The state of the comparison worker |
| `spent_today_usd` | The provider spend of calibration today |
| `daily_usd` | The daily provider budget of calibration |
| `auto_apply` | `true` if the profiles apply allowed proposals by themselves |
| `proposals` | The rule proposals |

 When the profiles set `auto_apply`, the System API applies the allowed proposals every 10 minutes by itself.

## Integrations

| Method and path | Body | Response | What it does |
|---|---|---|---|
| `GET /integrations/huggingface` | None | `configured`, `user`, `set_at` | The Hugging Face account that the token belongs to. |
| `PUT /integrations/huggingface` | `{"token": "hf_..."}` | `configured`, `user`, `set_at` | Checks the token with Hugging Face and stores it. Searches and downloads use it at once. A refused token returns `422`. |
| `DELETE /integrations/huggingface` | None | `{"configured": false}` | Deletes the token. |
| `GET /integrations/open-webui` | None | `configured`, `embedding_model`, `last_sync`, `last_sync_at`, `error` | The Open WebUI connection and the result of the last sync. |
| `PUT /integrations/open-webui` | `{"api_key"}` | The same as `GET` | Stores an Open WebUI admin API key, after a check against an admin-only endpoint, and syncs. |
| `DELETE /integrations/open-webui` | None | The same as `GET` | Deletes the stored Open WebUI key. |
| `POST /integrations/open-webui/connect` | `{"email", "password", "create_admin", "name"}` | The same as `GET` | Signs in to Open WebUI as an admin, or creates the first admin when `create_admin` is `true`. It turns on API keys, gets the admin's key and stores only the key. The password is not stored. |
| `GET /integrations/arcane` | None | `user`, `password`, `error` | The Arcane admin account. The password shows only while it still works. |

The Open WebUI sync points the model connection of Open WebUI at the router, and its document embeddings at the first installed embedding model. It runs after every configuration change, when a key is stored.

At the first start, the System API replaces Arcane's default password with a random one. `GET /integrations/arcane` shows it. After the owner changes the password in Arcane, the System API deletes its copy and shows no password.

## Other endpoints on port 8443

Port 8443 also serves the inference APIs. They take a router client key, not the System API key. [API clients](../guides/api-clients.md) explains how to call them.

| Path | Served by | Content |
|---|---|---|
| `GET /health` | Caddy | Returns `OK`. |
| `GET /v1/models` | Router | The models and profiles that the calling client can use |
| `POST /v1/chat/completions` | Router | OpenAI chat completions |
| `POST /v1/embeddings` | Router | OpenAI embeddings |
| `POST /v1/messages` | Router | Anthropic Messages |
| `POST /v1/messages/count_tokens` | Router | The router's own token estimate |
| Other `/v1/*` paths | Router | Sent to llama-swap. These requests name a local model and never leave the host. |
| `/api/*` | System API (Ollama shim) | The Ollama-compatible API: `version`, `tags`, `show`, `ps`, `chat`, `generate`, `embed`, `embeddings` |
| `/console/*` | System API | The web console |

The router also has two routes that only the System API key can call: `POST /sibyl/explain` and `POST /sibyl/classify`. Caddy does not expose them. Call `POST /router/explain` and `POST /router/classify` on the System API instead.
