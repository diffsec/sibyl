# Smart model router (phase 6)

Status: design, not built. Written on 2026-10-28, after phases 1 to 5.

The router is a gateway in front of llama-swap. It serves a request on the local GPUs when a local model can do the job, and sends it to a frontier provider (Anthropic, OpenAI, OpenRouter and similar) when the request needs more context, more capability, or a feature that the local model lacks. Rules that the owner writes decide each request. A small local classifier can add labels, such as the task and its difficulty, for the rules to use.

The router is off by default. With no provider configured, it passes every request to llama-swap, and Sibyl behaves as it does today. No request content leaves the host unless the owner configures a provider and a rule that allows it.

## Goals and limits

Goals:

- One OpenAI-compatible endpoint for clients. A client asks for `auto` and gets the right model, or names a model and gets that one.
- Rules that a person can read, test and change, with the reason for each decision recorded.
- Hard privacy controls: a client or a request can be kept on the host, and nothing widens that.
- Cost controls: price tables, per-client budgets, and a local fallback when a budget runs out.
- Low overhead: a local request through the router adds under 5 ms when no classifier runs.

Limits:

- The router routes whole requests. It does not split one request across models, and it does not cascade a request through several models to compare answers.
- It does not change the content of a request, except where a provider's API needs another shape.
- The first release supports the OpenAI chat completions and embeddings formats on the client side. The OpenAI Responses API and the Anthropic Messages format on the client side come later, if clients need them.

## Where it runs

The router is a new Go service, `cmd/sibyl-router` with `internal/router`, in a new rootless container `sibyl-router` on the `sibyl` network.

```
client --TLS--> caddy :8443 /v1/* --> sibyl-router --> llama-swap (local)
                                            |
                                            +--> providers over HTTPS (Anthropic, OpenAI, OpenRouter, ...)
Open WebUI --------------------------------> sibyl-router
System API Ollama shim (/api/*) -----------> sibyl-router
```

- Caddy sends `/v1/*` to the router instead of llama-swap. The router checks the client key, so Caddy no longer compares a bearer token for `/v1`.
- Open WebUI and the Ollama shim call the router, not llama-swap, so the same policies apply to every path. Each gets its own client key.
- llama-swap stays unreachable from outside the `sibyl` network.

The router is not part of the System API, for two reasons. The System API holds administrative power over the host, and the router handles untrusted request bodies at high volume. Keeping them apart limits what a bug in either one exposes. It also completes the key split from the security audit: inference clients get router keys, and only operators get the System API key.

## Clients and keys

The router owns the inference keys. Each client has a name, a key, a policy and an optional budget.

```yaml
# /var/lib/sibyl/config/router/clients.yaml, written by the System API
clients:
  - name: open-webui
    key_sha256: 9f2c...        # the router stores only the hash
    policy: hybrid
  - name: laptop-agents
    key_sha256: 41ab...
    policy: hybrid
    budget: {daily_usd: 5, monthly_usd: 50}
  - name: home-automation
    key_sha256: 77de...
    policy: local-only
```

- `local-only` clients never reach a provider, whatever the rules or the request say.
- `hybrid` clients follow the rules.
- The System API creates and revokes keys (`POST /router/clients`, `DELETE /router/clients/{name}`) and shows a new key once. The TUI gets a Router screen for this.
- The key that Sibyl creates at the first boot becomes the first client, `default`, with the `local-only` policy, so an upgrade changes nothing for existing clients.

## Model names

| A client asks for | The router does |
|---|---|
| A local model ID, such as `qwen3-32b-q4_k_m` | Sends it to llama-swap, as today |
| `<provider>/<model>`, such as `anthropic/claude-sonnet-5-5` | Sends it to that provider, if the client's policy allows providers |
| `auto` | Runs the rules of the `auto` profile |
| `auto:<profile>`, such as `auto:coding` | Runs the rules of that profile |

`GET /v1/models` lists, for the calling client, the local models, the provider models that its policy allows, and the profiles.

## The request profile

Before the rules run, the router computes a profile of the request. Only local work makes the profile. No content leaves the host to make it.

| Field | Source |
|---|---|
| `client`, `policy` | The client key |
| `prompt_tokens` | An estimate from the message text, with a margin. The router asks llama-server's `/tokenize` only when the estimate is near a limit. |
| `max_tokens`, `stream` | The request |
| `has_images`, `has_tools`, `json_schema` | The request |
| `local_context` | The context size of the local model that the profile names, from `models.yaml` |
| `local_busy` | llama-swap's `/running`: whether the local model is loading or serving another request |
| `sensitive` | Local detectors: private keys, API tokens, credential patterns, and owner-listed words |
| `task`, `difficulty` | The classifier, when one is configured. Otherwise `unknown`. |
| `hint` | The `X-Sibyl-Route` request header: `local`, `cloud` or `auto` |

A `hint` can narrow a decision to `local`. It can ask for `cloud` only when the client's policy sets `allow_hint: true`.

## Rules

A profile is an ordered list of rules. The first rule whose conditions all match decides the route. Conditions are typed fields, not an expression language, so the System API can check every rule when it is saved, and each condition has a test.

```yaml
# /var/lib/sibyl/config/router/profiles.yaml
profiles:
  auto:
    local_model: qwen3-32b-q4_k_m
    rules:
      - name: secrets stay on the host
        when: {sensitive: true}
        route: local
      - name: too long for the local model
        when: {prompt_tokens_over: local_context}
        route: anthropic/claude-sonnet-5-5
      - name: hard coding work
        when: {task: [code], difficulty: [hard]}
        route: anthropic/claude-opus-5-5
      - name: images need a vision model
        when: {has_images: true}
        route: openrouter/<vision model>
      - name: everything else
        route: local
    fallback: [local, anthropic/claude-haiku-5-5]
```

- `route: local` means the profile's `local_model`. A rule can also name a local model ID.
- `fallback` is the order that the router tries when the chosen route fails. A `local-only` client skips every provider in it.
- A rule that names a provider for a `local-only` client does not match, and the next rule runs.
- `POST /router/explain` takes a request body and returns the profile, every rule with match or no match, and the route, without sending the request. The TUI uses it to test rules.

## Classifier

The classifier adds `task` and `difficulty` labels. It is optional, and the rules work without it. Two kinds are planned, and the owner picks one.

| Kind | How it works | Cost |
|---|---|---|
| `embedding` (recommended) | Embeds the last user message with the embedding model that Open WebUI already keeps loaded, and takes the labels of the nearest examples from a labelled set. Sibyl ships a starter set, and the owner can add examples from the decision log. | One embedding call, tens of milliseconds. No extra VRAM. |
| `llm` | Asks a small local model, such as a 1 to 2B instruct model, for the labels, constrained to a JSON schema. | One short generation, 100 to 500 ms, and VRAM for the model. |

The classifier has a time limit of 300 ms. When it runs out, the labels are `unknown`, and the rules continue. A classifier error never fails a request.

Published work supports a learned router. LMSYS reports that its matrix factorization router reached 95% of GPT-4's quality on MT Bench with 26% of the calls going to GPT-4 ([RouteLLM](https://lmsys.org/blog/2024-07-01-routellm/)). The same kind of router can be pushed to the expensive model by text written for that purpose ([Rerouting LLM Routers](https://arxiv.org/pdf/2501.01818)). In Sibyl the budgets limit that cost, because a client's own requests can only spend that client's budget.

## Providers

Each provider has a type, a base URL, a key, and a price table.

```yaml
# /var/lib/sibyl/config/router/providers.yaml (keys are in separate 0600 files)
providers:
  anthropic:
    type: anthropic
  openai:
    type: openai
    base_url: https://api.openai.com/v1
  openrouter:
    type: openai
    base_url: https://openrouter.ai/api/v1
```

### OpenAI-compatible providers

OpenAI, OpenRouter and any service with an OpenAI-compatible API use one adapter. It rewrites the model name, removes the Sibyl headers, sets the provider key, and passes the body and the stream through.

### Anthropic

The Anthropic adapter uses the official Go SDK, `github.com/anthropics/anthropic-sdk-go`, and the Messages API. It translates between the OpenAI chat format and the Messages format.

| OpenAI chat | Anthropic Messages |
|---|---|
| `system` and `developer` messages | The top-level `system` field |
| `tools` with JSON Schema parameters | `tools` with `input_schema` |
| An assistant `tool_calls` entry | A `tool_use` content block |
| A `tool` role message | A `tool_result` block in a user message |
| An image `data:` URL | An `image` content block |
| `max_tokens` or `max_completion_tokens` | `max_tokens`, which the Messages API requires. The default is 16000, or 64000 for a streamed request. |
| `reasoning_effort` | `output_config.effort` |
| `temperature`, `top_p` | Dropped. API version `2026-10-21` rejects `temperature`, `top_p` and `top_k` on every model. The response carries a header that says which fields were dropped. |

A streamed response arrives as Messages API events. The adapter turns each text delta into an OpenAI `delta.content` chunk, and each tool input delta into a `tool_calls` arguments chunk. The final event's stop reason maps to `finish_reason`: `end_turn` to `stop`, `max_tokens` to `length`, `tool_use` to `tool_calls`, and `refusal` to `content_filter`. Usage goes into the last chunk, as OpenAI clients expect with `stream_options.include_usage`.

The adapter also sets these, because they cost nothing in quality:

- Automatic prompt caching (`cache_control` at the top level), so a long system prompt or conversation is billed at the cache read price on the next turn.
- The server-side refusal fallback on the models that support it, so a request that a safety classifier declines is answered by the fallback model in the same call.

### Prices and budgets

The router computes the cost of each provider request from the usage that the provider returns and a price table in `providers.yaml`. Sibyl ships a dated table for the Anthropic models, and the owner keeps it current. For example, on 2026-10-06 Anthropic listed Claude Sonnet 5.5 at $2 per million input tokens and $10 per million output tokens.

A client with a budget is checked before each provider request. When the budget is spent, the router uses the profile's local route, or returns 429 if the request cannot run locally. Usage totals per client and per day are kept in `/var/lib/sibyl/config/router/usage.json`.

## Failures

- Local model loading or busy: the router waits up to the profile's `local_wait` (default 30 seconds), then tries the fallback list.
- Provider 429 or 5xx: one retry after the `retry-after` delay or a short backoff, then the next fallback.
- A streamed response falls back only before the first byte reaches the client. After that, the router ends the stream with an error chunk.
- The provider's own error text goes back to the client, with the provider and the rule named in the response headers.

## What the owner sees

Every response carries `X-Sibyl-Route`, such as `anthropic/claude-sonnet-5-5; rule="too long for the local model"`.

The decision log, `/var/lib/sibyl/config/router/decisions.jsonl`, records one line per request: time, client, requested model, route, rule, token counts, cost and latency. It never records message content. It rotates at 50 MB.

The System API reads the log for `GET /router/stats`, and the TUI's Router screen shows requests, routes, spend per client, and the last decisions.

## Security

- The router is off until a provider exists. The first boot creates only the `default` client, with the `local-only` policy.
- Provider keys are files with mode 0600 in `/var/lib/sibyl/config/router/keys/`. The System API checks a key with the provider before it stores it, as it does for the Hugging Face token. The router never logs a key.
- Client keys are stored as SHA-256 hashes and compared in constant time.
- The router container drops all capabilities, runs read-only, and has no access to the Podman socket or the host queue directories.
- `local-only` is enforced in the router, after the rules. A rule bug cannot send a `local-only` client's request to a provider. A test covers every rule type against a `local-only` client.
- Outbound connections verify TLS with the system trust store, and honour `HTTPS_PROXY`.

## Delivery

| Step | Scope | Exit check |
|---|---|---|
| 6.1 | Router service with client keys and policies, model names, the OpenAI-compatible adapter, typed rules, `explain`, the decision log. Caddy, Open WebUI and the Ollama shim move to the router. | With no provider, every existing client works unchanged. A `local-only` client's request never leaves the host: a packet capture on the host shows no connection to a provider. |
| 6.2 | Anthropic adapter, price tables, budgets, fallbacks, the TUI Router screen. | A request longer than the local context goes to the configured provider with tool calls and streaming intact. A spent budget sends the next request to the local model. |
| 6.3 | Classifier, starter example set, and an evaluation script that replays a labelled request set and reports route, cost and quality per profile. | On the labelled set, the `auto` profile sends fewer requests to providers than an all-provider profile, at a quality that the owner accepts. |

## Open questions

These need the owner's decision before step 6.1 starts:

1. Which providers come first: Anthropic and OpenRouter, or OpenAI as well?
2. Does Open WebUI start as `local-only` or `hybrid`? `local-only` keeps today's behaviour.
3. Should the router also accept the Anthropic Messages format from clients, for tools that speak only that API?
4. What default budget does a new `hybrid` client get: none, or a small daily cap?
