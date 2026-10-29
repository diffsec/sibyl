# Smart model router (phase 6)

Status: steps 6.1 and 6.2 are built (2026-10-29). Step 6.3 is design. Written on 2026-10-28, after phases 1 to 5.

The router is a gateway in front of llama-swap. It serves a request on the local GPUs when a local model can do the job, and sends it to a frontier provider (Anthropic, OpenAI, OpenRouter and similar) when the request needs more context, more capability, or a feature that the local model lacks. Rules that the owner writes decide each request. A small local classifier can add labels, such as the task and its difficulty, for the rules to use.

With no provider configured, the router passes every request to llama-swap, and Sibyl behaves as it did before the router. No request content leaves the host unless the owner configures a provider and a rule that allows it.

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
- The first release accepts three formats from clients: OpenAI chat completions, OpenAI embeddings, and Anthropic Messages. The OpenAI Responses API comes later, if clients need it.

## Where it runs

The router is a Go service, `cmd/sibyl-router` with `internal/router`. It runs in the rootless container `router`, from the image `sibyl-router`, on the `sibyl` network at `router:8080`.

```
client --TLS--> caddy :8443 /v1/* --> router --> llama-swap (local)
                                      |
                                      +--> providers over HTTPS (Anthropic, OpenAI, OpenRouter, ...)
Open WebUI --------------------------------> router
System API Ollama shim (/api/*) -----------> router
```

- Caddy sends `/v1/*` to the router instead of llama-swap, and `/api/*` to the System API's Ollama shim. Caddy compares no key: the router and the shim check them.
- Open WebUI calls the router with its own client key, `open-webui`. The Ollama shim checks the caller's key with the router (`GET /v1/models`, kept for a minute) and sends it on, so the caller's policy applies. The shim refuses the System API key.
- llama-swap stays unreachable from outside the `sibyl` network.

The router is not part of the System API, for two reasons. The System API holds administrative power over the host, and the router handles untrusted request bodies at high volume. Keeping them apart limits what a bug in either one exposes. It also completes the key split from the security audit: inference clients get router keys, and only operators get the System API key.

## Clients and keys

The router owns the inference keys. Each client has a name, a key, a policy and an optional budget.

```yaml
# /var/lib/sibyl/config/router/clients.yaml, written by the System API
clients:
  - name: open-webui
    key_sha256: 9f2c...        # the router stores only the hash
    policy: local-only
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
- The first client is `default`, with the `local-only` policy. Its key is `sibyl-inference-key`, which operators read in `/var/lib/sibyl-operator/inference.key`. Until 2026-10-29 the System API key was also the `default` key. Since then the System API key is not an inference key, so a program that used `api.key` for `/v1` needs `inference.key` or a client key of its own.
- Open WebUI starts as the client `open-webui` with the `local-only` policy. The admin changes it to `hybrid` to let Open WebUI use providers.
- The keys of `default` and `open-webui` are Podman secrets (`sibyl-inference-key` and `sibyl-router-owui-key`), not hashes in `clients.yaml`. An entry for either name in `clients.yaml` sets only its policy. The secrets service creates a missing secret at every boot, so an upgraded host gets the Open WebUI key before the services start.
- Open WebUI keeps its connection settings in its database after the first start. The System API therefore moves an older install from llama-swap to the router through Open WebUI's admin API, when an Open WebUI admin key is stored. Until then, Open WebUI keeps calling llama-swap directly.
- The router reloads its configuration files within two seconds of a change. A file with an error is not used: the router keeps the last good configuration and logs the error.

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
| `prompt_tokens` | An estimate: the characters of the message text, tool calls and tool definitions, divided by 3. English text runs near 4 characters per token, so the estimate is high, and a request near the local context goes to the larger context. The design asked llama-server's `/tokenize` near a limit. That call is not made, because it loads the model in llama-swap. |
| `max_tokens`, `stream` | The request |
| `has_images`, `has_tools`, `json_schema` | The request |
| `local_context` | The context size of the local model that the profile names. The System API writes it to `models.json` on each apply: the configured context, else the model file's native context. |
| `local_ready` | llama-swap's `/running`: whether the local model is loaded and ready. The router asks only when a rule tests it. llama-swap does not report the requests in flight, so "busy" is not known. |
| `sensitive` | Local detectors: private keys, API tokens, credential patterns, and the owner's `sensitive_words` |
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
- `local_wait_seconds` (default 30) limits the wait for a local route, but only when a next route exists. With no next route, the router waits as long as llama-swap takes to load the model.
- Conditions: `sensitive`, `has_images`, `has_tools`, `json_schema`, `stream` and `local_ready` take true or false. `prompt_tokens_over` takes a number or `local_context`. `max_tokens_over` takes a number. `task`, `difficulty`, `hint` and `clients` take a list of values.
- A rule that names a provider for a `local-only` client does not match, and the next rule runs.
- `POST /router/explain` on the System API takes `{"client", "format", "hint", "request"}` and returns the profile, every rule with match or no match, and the route, without sending the request. The System API asks the router's `/sibyl/explain`, which only the System API key can call.

## Classifier

The classifier adds `task` and `difficulty` labels. It is optional, and the rules work without it. Two kinds are planned, and the owner picks one.

| Kind | How it works | Cost |
|---|---|---|
| `embedding` (recommended) | Embeds the last user message with the embedding model that Open WebUI already keeps loaded, and takes the labels of the nearest examples from a labelled set. Sibyl ships a starter set, and the owner can add examples from the decision log. | One embedding call, tens of milliseconds. No extra VRAM. |
| `llm` | Asks a small local model, such as a 1 to 2B instruct model, for the labels, constrained to a JSON schema. | One short generation, 100 to 500 ms, and VRAM for the model. |

The classifier has a time limit of 300 ms. When it runs out, the labels are `unknown`, and the rules continue. A classifier error never fails a request.

Published work supports a learned router. LMSYS reports that its matrix factorization router reached 95% of GPT-4's quality on MT Bench with 26% of the calls going to GPT-4 ([RouteLLM](https://lmsys.org/blog/2024-07-01-routellm/)). The same kind of router can be pushed to the expensive model by text written for that purpose ([Rerouting LLM Routers](https://arxiv.org/pdf/2501.01818)). In Sibyl the budgets limit that cost, because a client's own requests can only spend that client's budget.

## Providers

Each provider has a type, a base URL, a key, and a price table. The type is `openai` or `anthropic`. An `anthropic` provider needs no base URL: the default is `https://api.anthropic.com`.

```yaml
# /var/lib/sibyl/config/router/providers.yaml (keys are in separate 0600 files)
providers:
  anthropic:
    type: anthropic
    prices:                    # optional: overrides the built-in table
      claude-sonnet-5-5: {input: 2, output: 10, cache_read: 0.20, cache_write: 2.5}
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
| `stop` | `stop_sequences` |
| `tool_choice` and `parallel_tool_calls: false` | `tool_choice`, with `disable_parallel_tool_use` |
| `temperature`, `top_p`, and any other field | Dropped. Current Claude models accept only the default sampling values, and API version `2026-10-21` rejects `temperature`, `top_p` and `top_k` on every model. The `X-Sibyl-Dropped` response header names each dropped field. |
| An assistant `reasoning_content` in the response | The text of the `thinking` blocks, when the model returns any |

Consecutive turns of the same role merge into one, because the Messages API wants the roles to alternate. Several `tool` messages after one assistant turn therefore become one user turn with several `tool_result` blocks. The SDK sends API version `2023-06-01` and does not retry: the router retries and falls back itself.

Thinking blocks do not survive the OpenAI chat format, so the next turn of a conversation goes back without them. Claude models with preserved thinking accept that: a history with no thinking block has no block to check. The reasoning of earlier turns is not replayed, which costs some quality in a long tool loop. A client that needs it uses the Messages endpoint, which passes the thinking blocks through.

A streamed response arrives as Messages API events. The adapter turns each text delta into an OpenAI `delta.content` chunk, each thinking delta into `delta.reasoning_content`, and each tool input delta into a `tool_calls` arguments chunk. The final event's stop reason maps to `finish_reason`: `end_turn` to `stop`, `max_tokens` to `length`, `tool_use` to `tool_calls`, and `refusal` to `content_filter`. The usage goes into its own last chunk when the client sets `stream_options.include_usage`, and into the finish chunk otherwise, so the router can always count the cost. The adapter reads the first event before it answers, so a refused or overloaded request still reaches the fallback list.

The adapter also sets these, because they cost nothing in quality:

- Automatic prompt caching (`cache_control` at the top level), so a long system prompt or conversation is billed at the cache read price on the next turn.

The design also named the server-side refusal fallback. It is not built: it needs the SDK's beta message types throughout the adapter, and only Claude Fable 5.1 supports it. A refusal comes back as `finish_reason: content_filter`.

### Anthropic Messages from clients

The router also serves `POST /v1/messages`, so a tool that speaks only the Anthropic API can use Sibyl. The router reads the request into the same form as an OpenAI chat request, with the table above in reverse, and the rules see one form. The response goes back as Messages API events.

- An `anthropic` route gets the client's request body unchanged, apart from the model name and the key. The client's `anthropic-version` and `anthropic-beta` headers go with it. No field is lost in translation, and the response goes back as the provider sent it.
- A local route or an OpenAI-compatible route gets the translated request.
- The client's `x-api-key` header carries the Sibyl client key. The router never passes it to a provider.
- On a local or OpenAI-compatible route, thinking blocks in the request are dropped, and the response has none. Server tools, such as web search, have no input schema, so they are left out of the request.
- `POST /v1/messages/count_tokens` returns the router's own estimate, the same one that the rules use.

### Prices and budgets

The router computes the cost of each provider request from the usage that the provider returns and a price table. Prices are US dollars per million tokens: `input`, `output`, `cache_read` and `cache_write`. `long_above`, `long_input` and `long_output` set the prices of a prompt over a size, as Claude Haiku 5.5 has.

- A provider's `prices` in `providers.yaml` come first.
- For the type `anthropic`, a built-in table follows (`AnthropicPrices` in `internal/router/price.go`). It holds the prices that Anthropic listed on 2026-10-29. For example, Claude Sonnet 5.5 is $2 per million input tokens and $10 per million output tokens. Where the list gave no cache price, a cache read is 0.1 times and a cache write 1.25 times the input price. The owner keeps the table current by overriding a price in `providers.yaml`.
- A model with no price has an unknown cost. Its log entries carry `"unpriced": true`, and the Router screen counts them.

Each decision log entry carries the cached tokens and `cost_usd`.

A new client has no budget, so its provider spend has no limit until the admin sets one. The Router screen shows each client's spend, and a `hybrid` client with no budget is marked there. A budget is `{daily_usd, monthly_usd}`. A day and a month are UTC.

A client with a budget is checked before each provider request:

- When the budget is spent, the router skips every provider route. A request for a profile then runs on the profile's local model, even when the fallback list does not name it. A request that names a provider model gets 429.
- A model with no price is skipped too, with 429 if nothing else is left, because its cost cannot be counted against the budget.
- The check happens before the request, and the cost is known after it. Requests that run at the same time can therefore go over a budget by the cost of those requests.

The router keeps the spend per client and per day in `/var/lib/sibyl/config/router/log/usage.json` for 62 days.

## Failures

- Local model loading or busy: the router waits up to the profile's `local_wait` (default 30 seconds), then tries the fallback list.
- Provider 429 or 5xx: one retry after the `retry-after` delay or a short backoff, then the next fallback.
- A streamed response falls back only before the first byte reaches the client. After that, the router ends the stream with an error chunk.
- The provider's own error text goes back to the client, with the provider and the rule named in the response headers.

## What the owner sees

Every response carries `X-Sibyl-Route`, such as `anthropic/claude-sonnet-5-5; rule="too long for the local model"`.

The decision log, `/var/lib/sibyl/config/router/log/decisions.jsonl`, records one line per request: time, client, requested model, route, rule, status, token counts, cost and latency. It never records message content. It rotates at 50 MB and keeps one old file.

The System API reads the log for `GET /router/stats`, and the TUI's Router screen shows requests, routes, spend per client, and the last decisions.

## Security

- No request leaves the host until the owner adds a provider and makes a client `hybrid`. The built-in clients start `local-only`.
- Provider keys are files with mode 0600 in `/var/lib/sibyl/config/router/keys/`. The System API checks a key with the provider before it stores it, as it does for the Hugging Face token. The router never logs a key.
- Client keys are stored as SHA-256 hashes and compared in constant time.
- The router container drops all capabilities, runs read-only, and has no access to the Podman socket or the host queue directories. It mounts its configuration read-only, and writes only the log directory.
- `local-only` is enforced in the router, after the rules. A rule bug cannot send a `local-only` client's request to a provider. A test covers every rule type against a `local-only` client.
- Outbound connections verify TLS with the system trust store, and honour `HTTPS_PROXY`.

## Delivery

| Step | Scope | Exit check |
|---|---|---|
| 6.1 (built) | Router service with client keys and policies, model names, the OpenAI-compatible adapter for OpenAI and OpenRouter, the Anthropic Messages endpoint for clients, typed rules, `explain`, the decision log. Caddy, Open WebUI and the Ollama shim move to the router. | With no provider, every existing client works unchanged. A `local-only` client's request never leaves the host: a packet capture on the host shows no connection to a provider. |
| 6.2 (built) | Anthropic adapter, price tables, budgets, fallbacks, the TUI Router screen. | A request longer than the local context goes to the configured provider with tool calls and streaming intact. A spent budget sends the next request to the local model. |
| 6.3 | Classifier, starter example set, and an evaluation script that replays a labelled request set and reports route, cost and quality per profile. | On the labelled set, the `auto` profile sends fewer requests to providers than an all-provider profile, at a quality that the owner accepts. |

## Decisions

The owner answered the design questions on 2026-10-29:

1. The first providers are Anthropic, OpenAI and OpenRouter.
2. Open WebUI starts as `local-only`, which keeps today's behaviour.
3. The router accepts the Anthropic Messages format from clients in step 6.1.
4. A new `hybrid` client has no default budget.
