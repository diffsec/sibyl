# Connect programs to the APIs

Programs and tools can use the models on Sibyl through three APIs: OpenAI, Ollama and Anthropic. This guide shows how to get a key and call each API.

## Addresses

| Service | Address | Use |
|---|---|---|
| Open WebUI | `https://<address>/` | Chat in the browser, with document search |
| OpenAI-compatible API | `https://<address>:8443/v1` | Programs and tools that speak the OpenAI API |
| Anthropic-compatible API | `https://<address>:8443/v1/messages` | Programs and tools that speak the Anthropic Messages API |
| Ollama-compatible API | `https://<address>:8443/api` | Programs and tools that speak the Ollama API |
| Arcane | `https://<address>:9443/` | Other containers on the same host |

The examples use `--cacert sibyl-ca.crt`, the certificate authority of the host. [First boot](../getting-started/first-boot.md) explains how to get it and trust it.

## The API keys

Sibyl has two kinds of key:

- An inference key belongs to one client of the model router. A client is a named user of the APIs, such as one program. An inference key works for the OpenAI, Anthropic and Ollama APIs, and nothing else.
- The System API key manages models, accounts, disks and the router. The `sibyl` console uses it. Do not give it to programs. It does not work for inference.

The first boot makes three keys with `openssl rand`. It stores them as Podman secrets of the `sibyl` user.

| Key | Secret | Use |
|---|---|---|
| System API key | `sibyl-api-key` | Copied to `/var/lib/sibyl-operator/api.key` for the console |
| Inference key of the client `default` | `sibyl-inference-key` | Copied to `/var/lib/sibyl-operator/inference.key` |
| Inference key of the client `open-webui` | `sibyl-router-owui-key` | Given to Open WebUI only |

The administrator and the operators can read both files in `/var/lib/sibyl-operator`.

### Get a key for a program

Give each program its own key. You can then see the requests and the spend of each program, and revoke one key without the others.

Make a client and its key in one of these places:

- The Router screen of the `sibyl` TUI: press `6`, then `n`.
- The Router page of the web console: open New client.
- The System API: `POST /system/router/clients`.

Sibyl stores only the hash of a client key, so it shows the key once. Copy it at once. To revoke a key, delete its client. [Use the router](router.md#clients-and-keys) explains clients, policies and budgets.

### Use the default key for a quick test

The `default` key is for a quick test. Read it on the host:

```bash
cat /var/lib/sibyl-operator/inference.key
```

From another computer, read it over SSH:

```bash
KEY="$(ssh admin@<address> cat /var/lib/sibyl-operator/inference.key)"
```

The examples below use the key in `$KEY`.

## Call the OpenAI API

1. Set the base URL to `https://<address>:8443/v1`.
2. Set the API key to the client key of the program.
3. Set the model to a name from `/v1/models`.

List the models:

```bash
curl --cacert sibyl-ca.crt -H "Authorization: Bearer $KEY" https://<address>:8443/v1/models
```

Send a chat request:

```bash
curl --cacert sibyl-ca.crt -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
  https://<address>:8443/v1/chat/completions \
  -d '{"model":"<model id>","messages":[{"role":"user","content":"Hello"}]}'
```

The router serves `/v1/chat/completions`, `/v1/embeddings` and `/v1/models`. It sends any other `/v1` request to the local models.

The first request to a model loads it into GPU memory. That takes from a few seconds to a few minutes for a large model.

## Call the Ollama API

1. Set the Ollama host to `https://<address>:8443`.
2. Send the client key as a bearer token in the `Authorization` header.

```bash
curl --cacert sibyl-ca.crt -H "Authorization: Bearer $KEY" https://<address>:8443/api/tags
```

A client that cannot send an `Authorization` header cannot use Sibyl. The Ollama API refuses the System API key.

Sibyl downloads models itself. `ollama pull` and the other model management calls therefore return an error that says so. [Models](models.md) explains how to download a model.

## Call the Anthropic API

Tools that speak the Anthropic Messages API, such as Claude Code, can use the local models.

1. Set the base URL to `https://<address>:8443`.
2. Set the API key to a client key.

Sibyl reads the key from the `x-api-key` header or from `Authorization: Bearer`.

```bash
curl --cacert sibyl-ca.crt -H "x-api-key: $KEY" -H "Content-Type: application/json" \
  https://<address>:8443/v1/messages \
  -d '{"model":"<model id>","max_tokens":200,"messages":[{"role":"user","content":"Hello"}]}'
```

Sibyl translates the request for the local model. Text, images, tools and streaming work. On a local model, thinking blocks are not returned, and server tools, such as web search, are left out of the request.

`POST /v1/messages/count_tokens` returns the router's own estimate of the prompt tokens.

## Model names

Every request goes through the model router. The model name in the request tells the router where to send it.

| Model name | Where the request goes |
|---|---|
| A local model ID, such as `qwen3-32b-q4_k_m` | That local model |
| `<local model>:<preset>`, such as `qwen3-32b-q4_k_m:coding` | That local model, with the sampling of the preset in the fields that the request leaves out |
| `<provider>/<model>`, such as `anthropic/claude-sonnet-5-5` | That cloud provider, if the client's policy allows providers |
| `auto` | The route that the rules of the `auto` profile choose |
| `auto:<profile>`, such as `auto:coding` | The route that the rules of that profile choose |

`GET /v1/models` lists the names that the calling client can use: the local models, the provider models that its policy allows, and the profiles. [Models](models.md) explains sampling presets. [Use the router](router.md) explains providers and profiles.

## Route headers

Each response has an `X-Sibyl-Route` header that names the route, such as `local/qwen3-32b-q4_k_m`. For a request to a profile, the header also names the profile and the rule that matched.

A program can send the `X-Sibyl-Route` header with a request:

| Value | Effect |
|---|---|
| `local` | Keeps this request on the host. Every client can send it. |
| `cloud` | Asks for a provider. It works only for a client with Allow the cloud hint turned on. |
| `auto` | The rules decide. This is the default. |

An OpenAI chat request that goes to an Anthropic provider loses the fields that the provider does not take, such as `temperature` and `top_p`. The `X-Sibyl-Dropped` response header names each dropped field.
