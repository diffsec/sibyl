# Smart model router (phase 6)

Status: steps 6.1, 6.2 and 6.3 are built (2026-10-29). The `llm` classifier is not built. Written on 2026-10-28, after phases 1 to 5.

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
| `<local model>:<preset>`, such as `qwen3-32b-q4_k_m:coding` | Sends it to the local model, with the preset's sampling in the fields that the request leaves out. The System API writes each model's preset values to `models.json`. |

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

### The embedding classifier as built

Step 6.3 built the `embedding` kind (`internal/router/classify.go`). The `llm` kind is not built.

- Configuration: the `classifier` section of `profiles.yaml` takes `model`, `k` (default 5), `timeout_ms` (default 300), `prefix` and `no_starter`. With no `model`, the classifier uses the first embedding model in `models.json`. On the test host that model is `qwen3-embedding-0.6b-q8_0`, which llama-swap keeps loaded next to every other model.
- Examples: 150 starter examples are built into the router (`internal/router/starter.jsonl`), 10 for each task and difficulty. The owner's examples are in `examples.jsonl` in the router's configuration directory, set with `PUT /router/examples`. They add to the starter set, and `no_starter: true` leaves the starter set out. An example with an unknown label, or more than 5,000 examples, stops the configuration from loading, and the router keeps the last good one.
- Index: the router embeds every example in batches of 32 and keeps the unit vectors in memory. A build starts after a configuration change when a profile has a `task` or `difficulty` rule, and otherwise on the first classification. A failed build retries after 2, 5, 15 and then every 60 seconds. Until the index is ready, the labels are `unknown`.
- Prefix: the classifier puts `prefix` before every text that it embeds, examples and requests alike. The default is `Instruct: Classify the kind of task and how hard it is for this request\nQuery: `, the instruction format of the Qwen3 embedding models. `prefix: ""` turns it off.
- A request: the classifier embeds the last user message that has text. A text over 1,200 characters keeps its first 800 and last 400 characters, because llama-server's default batch holds 512 tokens. The `k` nearest examples by cosine similarity vote, each with its similarity as the weight, and task and difficulty are voted on separately.
- Output: explain shows the labels, the neighbours with their similarities, and the time. The decision log keeps the labels but no text. `POST /router/classify` takes `{"text"}` and returns the labels and the state of the index.

The labels mean:

| Label | Meaning |
|---|---|
| `chat` | Conversation, advice and general knowledge |
| `code` | Write, read, debug or review code, shell commands, SQL or configuration |
| `math` | Calculation, algebra, statistics and proofs |
| `writing` | Compose, edit, rewrite or translate prose |
| `extraction` | Pull facts, fields or a summary out of text that the user gives |
| `easy` | One step and a short answer that a small model gets right |
| `medium` | Several steps or a longer answer, with standard knowledge |
| `hard` | Deep reasoning, many constraints, expert knowledge or a long design |

### Accuracy on the test host

The figures below come from `qwen3-embedding-0.6b-q8_0` on the test host, with the 150 starter examples and the 60 held-out requests of `tools/router-eval/requests.jsonl`. The same author wrote both sets, so the figures are likely higher than on real traffic. The owner's own examples are the remedy.

| Setting | Task right | Difficulty right | Hard or not right |
|---|---|---|---|
| No prefix, k 7 | 51 (85%) | 54 (90%) | 58 (97%) |
| Prefix, k 7 | 59 (98%) | 51 (85%) | 58 (97%) |
| Prefix, k 5 (the default) | 59 (98%) | 55 (92%) | 59 (98%) |
| Prefix, k 11 | 58 (97%) | 50 (83%) | 59 (98%) |

On the box, through the deployed router (`sha-2999ed5-amd`), `sibyl-eval` gave the same counts: task 59, difficulty 55. One classification took 12 ms at the median and 14 ms at the 95th percentile. For three profiles over the 60 requests, explain gave these routes:

| Profile | Local | Provider |
|---|---|---|
| `auto`: provider only for prompts over the local context | 60 | 0 |
| `auto` plus a rule that sends `difficulty: [hard]` to a provider | 41 | 19 |
| Every request to a provider | 0 | 60 |

The set holds 20 hard requests. The label rule sent 19 of them, and no easy or medium request, to the provider.

### Answer quality on the test host

On 2026-10-30, `sibyl-eval -send` sent the 60 requests twice on `sha-7f8c9bc-amd`. The first pass went to the local model `qwen3.8-27b-turbofcfusion-735-882-here-uncen-neo-coder-max-q8_0`. The second pass went to the provider model `orcarouter/deepseek/deepseek-v4.1-flash-1017`. Both models judged every answer from 1 to 10. A later `-rescore -retry` pass, with a limit of 32,768 output tokens, filled in the answers and scores that the first pass did not get. The score for the label rule takes the local answer for its 41 local routes and the provider answer for its 19 provider routes. The table gives the mean score with the number of scored answers in brackets, and the median latency.

| Answers | Judge | Easy | Medium | Hard | All | p50 latency |
|---|---|---|---|---|---|---|
| Every request local | DeepSeek | 9.60 (20) | 8.60 (20) | 7.05 (20) | 8.42 (60) | 21.7 s |
| Label rule, hard to the provider | DeepSeek | 9.60 (20) | 8.60 (20) | 8.37 (19) | 8.86 (59) | 16.8 s |
| Every request to the provider | DeepSeek | 9.85 (20) | 9.61 (18) | 8.74 (19) | 9.40 (57) | 5.5 s |
| Every request local | Local | 9.90 (20) | 8.40 (20) | 8.10 (20) | 8.80 (60) | 21.7 s |
| Label rule, hard to the provider | Local | 9.90 (20) | 8.40 (20) | 9.11 (19) | 9.14 (59) | 16.8 s |
| Every request to the provider | Local | 9.95 (20) | 9.40 (20) | 9.26 (19) | 9.54 (59) | 5.5 s |

The label rule closes 45% of the gap between local and provider with the DeepSeek judge, and keeps 68% of the requests local. The local model scores its own answers 0.4 points higher than DeepSeek does, so the step 6.4 default of the strongest provider model as judge is correct. The largest gap that remains is in medium requests: 8.60 local against 9.61 at the provider. Most of it is code. The four medium code requests scored 6.75 local and 9.50 at the provider. A rule that sends medium code requests to the provider is the next thing to measure. Local hard requests took 70 s at the median. One provider request, a persistent B+ tree in Rust, stopped at the token limit with no answer on every try. OrcaRouter returned no price for the model, so the run has no cost figure.

### The evaluation tool

`cmd/sibyl-eval` runs from the owner's computer against the box. It reads a labelled set (`tools/router-eval/requests.jsonl`, 60 requests that are not in the starter set). It sends each prompt to `POST /router/classify` and reports the accuracy and a confusion table for task and difficulty. For each profile, it asks `POST /router/explain` for the route and reports the local share and the estimated provider cost: the router's prompt token estimate times the input price, plus the request's `output_tokens` times the output price. Explain returns the price of a provider route for this. With `-send`, the tool sends the requests with an inference key, records the route that served each one, its usage, its cost and its latency, and writes the answers to `-out`. With `-judge <model>`, a model scores each answer from 1 to 10.

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
- The models.dev index follows. The System API fetches `https://models.dev/api.json` at start when its copy is missing or a day old, and checks again every hour. It keeps a trimmed copy, `config/router/catalog.json` (about 1.7 MB): the providers whose API the router speaks, with each model's prices, context tiers, limits and features. The router looks a model up under the provider's `catalog` field, a models.dev provider ID, or under the provider's own name. A failed fetch keeps the old copy, and so does a download that lost more than half of the providers. If the router cannot read a new copy, it keeps the prices of the last one. Only the host's address reaches models.dev. `SIBYL_MODELS_DEV_URL=` (empty) in the System API unit turns the fetch off.
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

## Continuous calibration (step 6.4)

Step 6.3 measures the routing once, on a fixed set that one author wrote. The result goes stale when the owner changes the local model, and it says nothing about the owner's real requests. Step 6.4 measures the routing all the time, on a small sample of real traffic, and turns the results into labelled examples and rule advice.

The first measurement on 2026-10-30 shows why. With Qwen3.8-27B TurboFCFusion as the local model, DeepSeek as judge gave the local answers 9.60 on easy, 9.00 on medium and 7.38 on hard requests. Only the hard requests showed a gap that is worth a provider call. Another local model or another workload moves that line.

### The loop

1. Sample. The router keeps a copy of a sampled request and its answer in `/var/lib/sibyl/config/router/calibration/`, with the labels, the route and the usage. The default rate is 2% of the requests of a client with `calibrate: true`. A `sensitive` request is never kept. The store keeps 14 days and at most 2,000 requests, and the oldest go first. This is the only place where Sibyl keeps message content, so the client setting is off by default, and `DELETE /router/calibration` empties the store.
2. Compare. A worker in the System API sends each sampled request to the other side: a local answer gets a provider answer, and a provider answer gets a local answer. The worker runs only when llama-swap has no request in flight, so calibration never delays a client. Provider calls count against a separate `calibration_daily_usd` budget, and the worker stops for the day when it is spent.
3. Judge. A judge model compares the two answers side by side, in a random order, and says which is better or that they tie. A comparison of two answers is steadier than a score from 1 to 10, and the random order removes the judge's bias toward the first answer. The judge does not see which model wrote which answer. When the judge is also one of the two models, the report says so, because a model tends to prefer its own answers. In the first measurement, the local model scored its own answers 0.3 to 0.8 points higher than DeepSeek did.
4. Learn. Each judged request becomes a labelled example with a measured outcome: `local_ok` is true when the local answer won or tied. The classifier gets a third label, `needs_provider`, voted by the nearest examples with an outcome, as the RouteLLM routers do. This label describes the owner's local model on the owner's own work, which the hand-written `difficulty` label cannot do.
5. Advise. For each task and difficulty, the System API reports the local win-or-tie rate, the provider cost per request, and the latency of each side. When a cell crosses a threshold, such as a local win-or-tie rate over 90% for 50 comparisons, the report proposes a rule change. The owner applies it with one key on the TUI Router screen.

### Signals that cost nothing

- Open WebUI keeps the thumbs up and down of its users. The System API can read them from Open WebUI's feedback API and join them to the sampled requests by time and model. A thumbs down on a local answer counts against `local_ok`.
- A client that sends the same request again within a minute is likely unhappy with the first answer. The router can count these regenerations per route without keeping any text.

### When the local model changes

The outcomes belong to one local model. When the profile's `local_model` changes, the old outcomes stop voting, and the System API runs the step 6.3 set against the new model as a first measurement. The calibration store then fills with comparisons for the new model.

### Limits

- A `local-only` client's requests never go to a provider for comparison. They can only be judged by a local judge, and the comparison then has no provider answer. The default is to not sample them.
- The judge is a model, so it can be wrong. The report shows the number of comparisons behind every figure, and a proposal needs at least 50.
- By default, rules change only when the owner accepts a proposal. The automatic mode applies a proposal only to a route that the owner already allows.

### As built, first part

The first part of step 6.4 builds the loop up to the judge and the report. The label `needs_provider`, the rule proposals, the Open WebUI feedback, the regeneration count and the remeasure after a model change come next.

- The router keeps the samples, and the comparison worker runs in the router, not in the System API. The router already holds the provider adapters, the spend file and the count of local requests in flight, which the worker needs. The router mounts `/var/lib/sibyl/config/router/calibration` read-write, mode 0700.
- Only `/v1/chat/completions` requests are sampled. A Messages request, and a request with tools or images, is not sampled, because its answer is not plain text that a judge can compare.
- Each sample is one JSON file, named by its time, with the request, both answers, the labels and the verdict. The worker fills the missing answer, and then the verdict, one step every 30 seconds or at once after a step that worked. It waits while a local request is in flight, and it never makes llama-swap load a model.
- Provider calls of calibration count in `usage.json` under `@calibration`, which is not a valid client name. A provider model with no price makes the worker wait, because its cost cannot be counted against `daily_usd`.
- `status.json` in the same directory holds the worker's last state and the reason it waits. A sample that fails three times is left alone.

### Decisions for step 6.4

The owner chose on 2026-10-30:

1. Sampling is opt-in per client with `calibrate: true`. It is off by default.
2. The strongest provider model judges by default. The owner can name another judge.
3. `calibration_daily_usd` defaults to 1.
4. A rule proposal waits for the owner by default. A setting lets Sibyl apply proposals by itself, only to routes that the owner already allows, and each change is logged.

## Delivery

| Step | Scope | Exit check |
|---|---|---|
| 6.1 (built) | Router service with client keys and policies, model names, the OpenAI-compatible adapter for OpenAI and OpenRouter, the Anthropic Messages endpoint for clients, typed rules, `explain`, the decision log. Caddy, Open WebUI and the Ollama shim move to the router. | With no provider, every existing client works unchanged. A `local-only` client's request never leaves the host: a packet capture on the host shows no connection to a provider. |
| 6.2 (built) | Anthropic adapter, price tables, budgets, fallbacks, the TUI Router screen. | A request longer than the local context goes to the configured provider with tool calls and streaming intact. A spent budget sends the next request to the local model. |
| 6.3 (built) | Classifier, starter example set, and an evaluation script that replays a labelled request set and reports route, cost and quality per profile. | On the labelled set, the `auto` profile sends fewer requests to providers than an all-provider profile, at a quality that the owner accepts. |
| 6.4 | Continuous calibration: sampled requests, comparison in idle time, a side-by-side judge, the `needs_provider` label from measured outcomes, rule proposals, Open WebUI feedback, and a remeasure when the local model changes. | After two weeks of use, the report gives a local win-or-tie rate for each task and difficulty with at least 50 comparisons. No sampled request is `sensitive`, and no request of a `local-only` client went to a provider. |

## Decisions

The owner answered the design questions on 2026-10-29:

1. The first providers are Anthropic, OpenAI and OpenRouter.
2. Open WebUI starts as `local-only`, which keeps today's behaviour.
3. The router accepts the Anthropic Messages format from clients in step 6.1.
4. A new `hybrid` client has no default budget.
