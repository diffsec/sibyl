# Configuration reference

This page lists every configuration file of Sibyl, the fields in each file, and the environment variables of the Sibyl programs. The guides tell you how to change these settings through the consoles. Use this page when you need the exact field names, types and limits.

The System API (the service that the consoles call) writes these files. It checks every value before it writes. You do not need to edit them by hand. [System API](system-api.md) lists the routes.

## Where the files are

The System API sees `/var/lib/sibyl/config` on the host as `/config`. The router sees `/var/lib/sibyl/config/router` as `/config`, read-only.

| File on the host | Written by | Content |
|---|---|---|
| `/var/lib/sibyl/config/models.yaml` | System API, `PUT /models/{id}` | [Model settings](#model-settings-modelsyaml) |
| `/var/lib/sibyl/config/hardware.yaml` | System API, `PUT /hardware` | [Hardware](#hardware-hardwareyaml) |
| `/var/lib/sibyl/config/llama-swap/config.yaml` | System API, from the model and hardware settings | The generated llama-swap configuration. Do not edit it. |
| `/var/lib/sibyl/config/downloads.json` | System API, download manager | The download jobs. They resume after a restart. |
| `/var/lib/sibyl/config/hf-token` | System API, `PUT /integrations/huggingface` | The Hugging Face access token, mode 0600. |
| `/var/lib/sibyl/config/huggingface.json` | System API, `PUT /integrations/huggingface` | The Hugging Face account name. |
| `/var/lib/sibyl/config/open-webui.key` | System API, `PUT /integrations/open-webui` | The Open WebUI admin API key that the System API calls Open WebUI with, mode 0600. |
| `/var/lib/sibyl/config/arcane-password` | System API | The password that replaces Arcane's default password, mode 0600. |
| `/var/lib/sibyl/config/router/clients.yaml` | System API, `/router/clients` routes | [Clients](#router-clients-clientsyaml) |
| `/var/lib/sibyl/config/router/providers.yaml` | System API, `PUT /router/providers/{name}` | [Providers](#router-providers-providersyaml) |
| `/var/lib/sibyl/config/router/keys/<provider>` | System API, `PUT /router/providers/{name}` | One provider's API key, mode 0600. |
| `/var/lib/sibyl/config/router/profiles.yaml` | System API, `PUT /router/profiles` | [Profiles](#router-profiles-profilesyaml) |
| `/var/lib/sibyl/config/router/examples.jsonl` | System API, `PUT /router/examples` | [Classifier examples](#classifier-examples-examplesjsonl) |
| `/var/lib/sibyl/config/router/models.json` | System API, when it applies the configuration | [Local models for the router](#local-models-for-the-router-modelsjson) |
| `/var/lib/sibyl/config/router/catalog.json` | System API, from the models.dev index | Provider model prices. |
| `/var/lib/sibyl/config/router/log/decisions.jsonl` | Router | The decision log. |
| `/var/lib/sibyl/config/router/log/usage.json` | Router | Provider spend per client and per UTC day. |
| `/var/lib/sibyl/config/router/calibration/` | Router | Calibration samples and results. |

The router reads its files again every 2 seconds. If a file has an error, the router logs it and keeps the last configuration that loaded. The router refuses unknown fields in its YAML files, so a typo in a rule does not pass unnoticed.

## Model settings (`models.yaml`)

`models.yaml` maps each model ID to its settings. The System API turns the settings into llama-server flags. Every field is optional. An empty field leaves Sibyl's or llama.cpp's default. [Models](../guides/models.md#change-a-models-settings) describes the settings in the form.

```yaml
qwen3-32b-q4_k_m:
  layout: span
  preset: coding
  reasoning: {var: enable_thinking, value: "off"}
  kv_cache: {type_k: q8_0, type_v: q8_0}
```

| Field | Type | Allowed values | llama-server flag | Meaning |
|---|---|---|---|---|
| `layout` | string | `span`, or `gpu:N` | `--split-mode`, `--device` | `span` spreads the model across the selected GPUs. `gpu:N` pins it to one GPU. N is a position in the selected GPU list, so it must be less than the number of selected GPUs. Empty uses the default layout in `hardware.yaml`. |
| `ctx_size` | integer | 0 or more | `--ctx-size` | The context size in tokens. 0 or empty lets Sibyl fit it to the GPUs. |
| `fit` | string | `quality` (default), `context` | `--ctx-size`, `--cache-type-k`, `--cache-type-v` | What an automatic context gives up when VRAM is short. `quality` keeps the KV cache types and shortens the context. `context` tries f16, then `q8_0`, then `q4_0` to keep the native context. |
| `embedding` | boolean | `true`, `false` | `--embedding` | Marks an embedding model. An embedding model with no `ctx_size` gets 8,192 tokens. |
| `hidden` | boolean | `true`, `false` | none | Leaves the model out of the model lists. |
| `preset` | string | `general`, `coding`, `agentic`, `writing` | `--temp`, `--top-k`, `--top-p`, `--min-p` | The sampling preset. The `sampling` fields override its values. |
| `sampling.temperature` | number | 0 to 5 | `--temp` | Default temperature. |
| `sampling.top_k` | integer | 0 to 1000 | `--top-k` | Default top-k. |
| `sampling.top_p` | number | 0 to 1 | `--top-p` | Default top-p. |
| `sampling.min_p` | number | 0 to 1 | `--min-p` | Default min-p. |
| `sampling.repeat_penalty` | number | 0 to 5 | `--repeat-penalty` | Default repeat penalty. |
| `sampling.presence_penalty` | number | -2 to 2 | `--presence-penalty` | Default presence penalty. |
| `reasoning.var` | string | `reasoning_effort`, `enable_thinking`, `thinking` | `--chat-template-kwargs` | The chat template variable that sets the reasoning. |
| `reasoning.value` | string | `on` or `off` for `enable_thinking` and `thinking`. For `reasoning_effort`, 1 to 32 letters, digits, `_` and `-`. | `--chat-template-kwargs` | The default reasoning. See [Reasoning settings](#reasoning-settings). |
| `parallel` | integer | 0 to 64 | `--parallel` | The number of requests that the model answers at once. The requests share the context. 0 leaves llama-server's default. |
| `tensor_split` | list of numbers | 0 or more each, at most one value per selected GPU | `--tensor-split` | The share of the layers on each GPU of a spanning model. Empty lets Sibyl split by the memory that each GPU has left. |
| `kv_cache.type_k` | string | `f32`, `f16`, `bf16`, `q8_0`, `q4_0`, `q4_1`, `iq4_nl`, `q5_0`, `q5_1` | `--cache-type-k` | The K cache type. |
| `kv_cache.type_v` | string | same as `type_k` | `--cache-type-v` | The V cache type. |
| `speculative.types` | list of strings | `none`, `draft-simple`, `draft-eagle3`, `draft-mtp`, `draft-dflash`, `draft-dspark`, `ngram-simple`, `ngram-map-k`, `ngram-map-k4v`, `ngram-mod`, `ngram-cache` | `--spec-type` | The speculative decoding types, tried in order. |
| `speculative.draft_model` | string | the ID of another installed model | `--spec-draft-model` | Required for `draft-simple`, `draft-eagle3`, `draft-dflash` and `draft-dspark`. Not allowed without one of them. |
| `speculative.draft_max` | integer | 1 to 64 | `--spec-draft-n-max` | The most draft tokens. |
| `speculative.draft_min` | integer | 0 to 64, at most `draft_max` | `--spec-draft-n-min` | The fewest draft tokens. |
| `speculative.ngram_mod_min` | integer | 1 to 64 | `--spec-ngram-mod-n-min` | For `ngram-mod`. |
| `speculative.ngram_mod_max` | integer | 1 to 64 | `--spec-ngram-mod-n-max` | For `ngram-mod`. |
| `moe.cpu_layers` | integer | -1 (all layers), 0 (off), or 1 to 4096 | `--n-cpu-moe`, or `--cpu-moe` for -1 | Keeps the expert weights of the first N layers in system RAM. |
| `moe.cache_mib` | integer | 0 (off) to 1,048,576 | `--moe-cache-mib` | A GPU cache, in MiB, for the experts in system RAM. Needs `moe.cpu_layers`. |
| `args` | list of strings | letters, digits and `_ . , : = / + -` | as given | Extra llama-server flags for anything that the typed fields do not cover. |

The System API also makes these checks:

- It refuses `moe` settings for a model file that has no experts.
- It refuses a `reasoning` value that the model's chat template does not take.
- It refuses a model as its own draft model.
- `args` cannot set a flag that Sibyl sets: `--model`, `-m`, `--host`, `--port`, `--device`, `-dev`, `--split-mode`, `-sm`, `--hf-repo`, `-hf`, `--api-key`, `--api-key-file`, `--chat-template-kwargs`.
- `args` cannot set a flag that has a typed field, or any alias of it. Set the typed field instead.

The flag names and the `--spec-type` values come from llama.cpp b11889.

`models.yaml` keeps only the values that you set. The values that Sibyl chooses, such as an automatic context, go into the llama-swap configuration and `models.json`. `GET /models` reports them as `fit`.

### Reasoning settings

Sibyl reads the chat template in the GGUF file to find its reasoning control. It looks for the variables in this order: `reasoning_effort`, `enable_thinking`, `thinking`.

| Template reads | Choices that the form offers | Flag value |
|---|---|---|
| `reasoning_effort` with listed levels | each level, such as `low`, `high` or `xhigh` | `{"reasoning_effort":"<level>"}` |
| `reasoning_effort` with no listed levels | any level, in a text field | `{"reasoning_effort":"<level>"}` |
| `enable_thinking` or `thinking` only | `on`, `off` | `{"<var>":true}` or `{"<var>":false}` |
| `reasoning_effort` and `enable_thinking` | each level, and `off` through `enable_thinking` | the level, or `{"enable_thinking":false}` |

Sibyl finds the levels where the template compares the variable with a value, tests it in a list, or gives it a default. It also follows the variable when the template copies it into another name with `{% set %}`. A request's own `reasoning_effort` or `chat_template_kwargs` wins over the setting.

In the form, the Reasoning field holds `var=value`, such as `reasoning_effort=low` or `enable_thinking=off`.

## Hardware (`hardware.yaml`)

`hardware.yaml` holds the GPU choices of the Hardware screen.

| Field | Type | Allowed values | Meaning |
|---|---|---|---|
| `backend` | string | `hip`, `cuda`, `vulkan`, as the image variant ships them | The llama.cpp GPU backend. The `amd` variant ships `hip` and `vulkan`. The `nvidia` variant ships `cuda` and `vulkan`. The `intel` variant ships `vulkan`. |
| `gpus` | list of integers | indexes into the detected GPU list | The selected GPUs. |
| `layout` | string | `span`, `pin` | The default placement. `span` spreads one model across every selected GPU. `pin` places each model on one GPU, so several models run at once. |

Before the setup wizard runs, Sibyl uses the variant's first backend on GPU 0 with `span`. On an AMD GPU that the HIP image does not support, it uses `vulkan`.

## Router clients (`clients.yaml`)

A client is a program or person that calls the inference APIs with its own key. [Router](../guides/router.md) shows how to manage clients.

```yaml
clients:
  - name: laptop
    key_sha256: <64 hex digits>
    policy: hybrid
    budget: {daily_usd: 2, monthly_usd: 20}
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| `name` | string | required | 1 to 32 lowercase letters, digits and dashes. It starts with a letter or digit. |
| `key_sha256` | string | required | The SHA-256 of the client's key, as 64 lowercase hex digits. The built-in clients `default` and `open-webui` have none, because their keys are Podman secrets. |
| `policy` | string | required | `local-only`: requests never reach a provider. `hybrid`: requests follow the rules. |
| `allow_hint` | boolean | `false` | Lets the client ask for a provider with the `X-Sibyl-Route: cloud` request header. Any client can send `X-Sibyl-Route: local`. |
| `budget.daily_usd` | number | no limit | The most provider spend per UTC day, in US dollars. 0 means no limit. Must not be negative. |
| `budget.monthly_usd` | number | no limit | The most provider spend per UTC month, in US dollars. Must not be negative. |
| `calibrate` | boolean | `false` | Lets the router keep a sample of the client's requests and answers for calibration. Only a `hybrid` client's requests are sampled. |

## Router providers (`providers.yaml`)

A provider is a frontier model service, such as OpenAI or Anthropic. The provider's API key is in `keys/<name>`, not in this file.

```yaml
providers:
  anthropic:
    type: anthropic
    models: [claude-sonnet-5-5]
  openrouter:
    type: openai
    base_url: https://openrouter.ai/api/v1
```

| Field | Type | Default | Meaning |
|---|---|---|---|
| (map key) | string | required | The provider name. Same rules as a client name. `local` and `auto` are reserved. |
| `type` | string | required | `openai` for any OpenAI-compatible API, or `anthropic` for the Anthropic Messages API. |
| `base_url` | string | `https://api.anthropic.com` for `anthropic` | The URL before the endpoint path, such as `https://api.openai.com/v1`. For `anthropic` it is the host. It must start with `https://`. |
| `models` | list of strings | empty | The model names that `GET /v1/models` lists for this provider. A client can name any other model of the provider too. |
| `prices` | map of model name to [price](#prices) | empty | Prices that override the models.dev prices and the built-in Anthropic prices. |
| `catalog` | string | the provider name | The provider's ID in the models.dev index, for prices. |

### Prices

All prices are in US dollars per million tokens, and must not be negative.

| Field | Type | Meaning |
|---|---|---|
| `input` | number | Input tokens. |
| `output` | number | Output tokens. |
| `cache_read` | number | Cached input tokens that the provider reads. |
| `cache_write` | number | Input tokens that the provider writes to its cache. |
| `long_above` | integer | A prompt size in tokens. A longer prompt is billed at `long_input` and `long_output`, and the cache prices scale with the input price. |
| `long_input` | number | Input tokens of a long prompt. |
| `long_output` | number | Output tokens of a long prompt. |

## Router profiles (`profiles.yaml`)

A profile is an ordered list of rules for the model name `auto` (the profile named `auto`) or `auto:<profile>`. The first rule whose conditions all match decides the route. [Router](../guides/router.md) shows how to write rules.

```yaml
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
      - name: everything else
        route: local
    fallback: [local, anthropic/claude-haiku-5-5]
```

### Top-level fields

| Field | Type | Default | Meaning |
|---|---|---|---|
| `sensitive_words` | list of strings | empty | Words that mark a request as sensitive, in addition to the built-in secret detectors. A word must not be empty. |
| `classifier` | [classifier](#classifier) | see below | Sets up the task and difficulty labels. |
| `calibration` | [calibration](#calibration) | see below | Sets up the comparison of sampled requests. |
| `profiles` | map of name to [profile](#profile) | required | The profiles. A name follows the client name rules. |

### Profile

| Field | Type | Default | Meaning |
|---|---|---|---|
| `local_model` | string | required | The local model that the route `local` means. |
| `rules` | list of [rules](#rule) | empty | The rules, in order. |
| `fallback` | list of routes | empty | The routes that the router tries, in order, when the chosen route fails. A `local-only` client skips every provider in it. |
| `local_wait_seconds` | integer | 30 | How long the router waits for a local model to start answering before it tries the next route. 0 to 3600. It applies only when a next route exists. |

### Rule

| Field | Type | Default | Meaning |
|---|---|---|---|
| `name` | string | required | A name for the decision log. |
| `when` | [conditions](#conditions) | empty | The conditions. A rule with no conditions always matches. |
| `route` | string | required | `local` (the profile's `local_model`), a local model ID, or `<provider>/<model>`. |

A rule that names a provider does not match for a `local-only` client, and the next rule runs.

### Conditions

A rule matches when every condition that it sets matches.

| Field | Type | Matches when |
|---|---|---|
| `sensitive` | boolean | The request has a secret or a sensitive word, or not. |
| `has_images` | boolean | The request has images, or not. |
| `has_tools` | boolean | The request has tools, or not. |
| `json_schema` | boolean | The request asks for a `json_schema` or `json_object` response format, or not. |
| `stream` | boolean | The request streams, or not. |
| `local_ready` | boolean | llama-swap has the profile's local model loaded, or not. |
| `needs_provider` | boolean | The vote of the nearest calibration outcomes says the request needs a provider, or not. The vote is unknown below 20 outcomes for the profile's local model, and then the condition matches neither `true` nor `false`. |
| `prompt_tokens_over` | integer or `local_context` | The prompt has more tokens than the number, or than the local model's context size. |
| `max_tokens_over` | integer | The request's `max_tokens` is more than the number. Must not be negative. |
| `task` | list of strings | The classifier's task label is in the list: `chat`, `code`, `math`, `writing`, `extraction`, `unknown`. |
| `difficulty` | list of strings | The classifier's difficulty label is in the list: `easy`, `medium`, `hard`, `unknown`. |
| `hint` | list of strings | The client's `X-Sibyl-Route` hint is in the list: `local`, `cloud`, `auto`. |
| `clients` | list of strings | The client's name is in the list. |

### Classifier

The `classifier` section sets up the task and difficulty labels. The router runs the classifier only for a profile with a `task` or `difficulty` rule.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `model` | string | the first embedding model in `models.json` | The local embedding model. |
| `k` | integer | 5 | The number of nearest examples that vote. 0 to 50. |
| `prefix` | string | `Instruct: Classify the kind of task and how hard it is for this request\nQuery: ` | Text that goes before each text that the classifier embeds, examples and requests alike. `""` means no prefix. At most 1000 bytes. |
| `timeout_ms` | integer | 300 | The time limit of one classification, in milliseconds. 0 to 10000. |
| `no_starter` | boolean | `false` | Leaves out the examples that ship with Sibyl, so only `examples.jsonl` counts. |

### Calibration

The `calibration` section sets up continuous calibration. The router keeps a sample of real requests, gets the answer of the other side (the local model or a provider) when the host is idle, and asks a judge model which answer is better.

| Field | Type | Default | Meaning |
|---|---|---|---|
| `sample_rate` | number | 0.02 | The share of a calibrating client's requests that the router keeps. 0 to 1. |
| `daily_usd` | number | 1 | The most provider spend of the comparisons and the judge per UTC day, in US dollars. 0 stops all provider calls. |
| `judge` | string | the provider model with the highest output price | The route of the judge model. |
| `compare` | string | the first provider route of the profile | The provider route that answers a request that the local model served. It must be a provider route. |
| `keep_days` | integer | 14 | How many days a sample is kept. 0 to 365. |
| `max_samples` | integer | 2000 | The most samples kept. The oldest go first. 0 to 100000. |
| `no_seed` | boolean | `false` | Stops the first measurement of a new local model with the labelled request set. |
| `auto_apply` | boolean | `false` | Lets the System API apply a rule proposal by itself, when the profile already names the proposal's route. |

A provider route needs a price for calibration, because the calibration budget counts its cost. The router keeps the calibration spend in `usage.json` under the name `@calibration`.

### Rule proposals

The calibration results give a rule proposal for a task and difficulty when these thresholds are met. These values are fixed in the code.

| Threshold | Value | Meaning |
|---|---|---|
| Comparisons | 50 | The comparisons that a task and difficulty need before they get a proposal. |
| Move to the local model | 0.9 | The local answer won or tied at least this often. |
| Move to a provider | 0.6 | The local answer won or tied less often than this. |

## Classifier examples (`examples.jsonl`)

`examples.jsonl` holds your own labelled examples for the classifier, one JSON object per line. Blank lines and lines that start with `#` are skipped.

```json
{"text": "Write a SQL query that lists the top ten customers by revenue", "task": "code", "difficulty": "medium"}
```

| Field | Type | Allowed values |
|---|---|---|
| `text` | string | Not empty. At most 4000 bytes. |
| `task` | string | `chat`, `code`, `math`, `writing`, `extraction` |
| `difficulty` | string | `easy`, `medium`, `hard` |

The file holds at most 5000 examples. Unknown fields are errors.

## Local models for the router (`models.json`)

The System API writes `models.json` when it applies the configuration. Do not edit it. It is a JSON list with one object per local model.

| Field | Type | Meaning |
|---|---|---|
| `id` | string | The model ID. |
| `context` | integer | The context size that llama-server runs the model with. With `parallel` set, it is the context of one request. |
| `embedding` | boolean | The model is an embedding model. |
| `presets` | map of preset to sampling | The sampling of each preset, for `<model>:<preset>` requests. Embedding models have none. |

## Environment variables

The Sibyl programs read these environment variables. The Quadlet units and the `sibyl` command set the values that differ from the defaults.

### System API (`sibyl-api`)

| Variable | Default | Meaning |
|---|---|---|
| `SIBYL_LISTEN` | `:3001` | The listen address. |
| `SIBYL_API_KEY_FILE` | `/run/secrets/api-key` | The file with the System API key. |
| `SIBYL_CONFIG_DIR` | `/config` | The configuration directory. |
| `SIBYL_MODELS_DIR` | `/models` | The models directory. |
| `SIBYL_POOLS_DIR` | `/pools` | The model pools directory. |
| `SIBYL_DROPIN_DIR` | `/quadlet` | The directory of the llama-swap Quadlet drop-in. |
| `SIBYL_RUN_DIR` | `/run/sibyl` | The directory of the host agent requests. |
| `SIBYL_HOST_OUT_DIR` | `/run/sibyl-host` | The directory of the `sibyl-hostd` results. |
| `SIBYL_AUTH_QUEUE` | `/run/sibyl-auth/req` | The queue of the web console password checks. |
| `SIBYL_SYSFS` | `/sys` | The sysfs root, for GPU detection. |
| `SIBYL_MEMINFO` | `/proc/meminfo` | The memory information file. |
| `SIBYL_REGISTRY` | `ghcr.io/diffsec` | The registry of the Sibyl images. |
| `SIBYL_IMAGE_TAG` | `latest` | The image tag. The unit sets it at build time. |
| `SIBYL_GPU_VARIANT` | `amd` | The GPU variant. The unit sets it at build time. |
| `SIBYL_VERSION` | `dev` | The version that the API reports. |
| `SIBYL_HF_TOKEN_FILE` | `/run/secrets/hf-token` | A Hugging Face token file, used when `hf-token` in the configuration directory is empty. |
| `SIBYL_MODELS_DEV_URL` | `https://models.dev/api.json` | The models.dev index for prices. An empty value turns the daily fetch off. |
| `SIBYL_ROUTER_URL` | `http://router:8080` | The router. |
| `SIBYL_LLAMA_SWAP_URL` | `http://llama-swap:8080` | llama-swap. |
| `SIBYL_OPEN_WEBUI_URL` | `http://open-webui:8080` | Open WebUI. |
| `SIBYL_ARCANE_URL` | `http://arcane:3552` | Arcane. |
| `SIBYL_CONSOLE_PROXY` | `caddy` | The host name of the proxy whose `X-Forwarded-For` header the web console trusts. |

### Router (`sibyl-router`)

| Variable | Default | Meaning |
|---|---|---|
| `SIBYL_LISTEN` | `:8080` | The listen address. |
| `SIBYL_API_KEY_FILE` | `/run/secrets/api-key` | The System API key, which the router accepts for its admin routes. |
| `SIBYL_DEFAULT_KEY_FILE` | `/run/secrets/default-key` | The key of the `default` client. Without it, that client is off. |
| `SIBYL_OPEN_WEBUI_KEY_FILE` | `/run/secrets/open-webui-key` | The key of the `open-webui` client. Without it, that client is off. |
| `SIBYL_ROUTER_CONFIG` | `/config` | The router configuration directory. |
| `SIBYL_ROUTER_LOG_DIR` | `/log` | The directory of `decisions.jsonl` and `usage.json`. |
| `SIBYL_ROUTER_CALIBRATION_DIR` | `/calibration` | The calibration directory. |
| `SIBYL_LLAMA_SWAP_URL` | `http://llama-swap:8080` | llama-swap. |

### TUI (`sibyl-tui`)

The `sibyl` command sets these values when it starts the TUI.

| Variable | Default | Value that `sibyl` sets | Meaning |
|---|---|---|---|
| `SIBYL_API_URL` | `http://system-api:3001` | `https://127.0.0.1:18443/system` | The System API address. |
| `SIBYL_CA_FILE` | none | `/run/secrets/sibyl-ca` | The CA certificate that the TUI checks the System API with. |
| `SIBYL_TLS_SERVER_NAME` | none | `sibyl.local` | The name that the TUI expects in the certificate. |
| `SIBYL_WIZARD` | none | `1` for `sibyl setup` | `1` starts the setup wizard. |

### Other programs

| Variable | Program | Meaning |
|---|---|---|
| `SIBYL_URL` | `sibyl-eval` | The default for `-url`: the server's API address, such as `https://192.168.1.26:8443`. |
| `SIBYL_INSTALL_DEBUG` | `sibyl-install` | `1` turns on debug output. |
