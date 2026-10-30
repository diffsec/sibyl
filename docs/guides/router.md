# Use the router

The model router decides where each API request runs. It sends a request to a local model, or to a cloud provider when you allow that. This guide shows how to set up clients, providers, budgets, profiles, rules and calibration.

You can do each task in three places:

- The Router screen of the `sibyl` TUI. Press `6` to open it.
- The Router page of the web console at `https://<address>:8443/console/router`.
- The System API at `https://<address>:8443/system/router`.

[The consoles](consoles.md) explains how to open the TUI and sign in to the web console. [Configuration](../reference/configuration.md) lists every field of the router files. [Router design](../design/router.md) explains how the router works inside.

## How the router decides

Every request to `/v1`, and every request to the Ollama API, goes through the router. The router checks the key of the request. The key tells it the client. Then the model name in the request decides the route.

- A local model ID goes to that local model.
- `<provider>/<model>` goes to that provider.
- `auto` and `auto:<profile>` run the rules of a profile.

With no provider configured, every request stays on the host. No request leaves the host until you add a provider and make a client `hybrid`.

[Connect programs to the APIs](api-clients.md#model-names) lists the model names.

## Use the System API

The API examples in this guide use the System API key in `$ADMIN`:

```bash
ADMIN="$(ssh admin@<address> cat /var/lib/sibyl-operator/api.key)"
```

To see the clients, the providers and the profiles, send `GET /system/router`:

```bash
curl --cacert sibyl-ca.crt -H "Authorization: Bearer $ADMIN" https://<address>:8443/system/router
```

[System API](../reference/system-api.md) lists every route.

## Clients and keys

A client is a named user of the APIs, such as one program. Each client has one inference key, a policy, and an optional budget. The policy is one of these:

| Policy | Effect |
|---|---|
| `local-only` | The client's requests never leave the host. This is the default. |
| `hybrid` | The routing rules can send the client's requests to a provider. |

Two clients exist from the start: `default` and `open-webui`. Both are `local-only`. Their keys are Podman secrets on the host. You can change their policy and budget, but you cannot delete them or make a new key for them with the router controls.

A client name has 1 to 32 lowercase letters, digits and dashes. It starts with a letter or a digit.

### Create a client

The answer shows the new key once. Sibyl stores only its hash and cannot show it again, so copy the key at once.

- TUI: press `n`. Give the name, the policy and the budget.
- Web console: open New client, fill in the form, and select Create and show the key.
- API:

```bash
curl --cacert sibyl-ca.crt -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  https://<address>:8443/system/router/clients -d '{"name":"laptop","policy":"local-only"}'
```

### Change a client

You can change the policy, the budget, Allow the cloud hint and Calibrate.

- Allow the cloud hint lets the client send `X-Sibyl-Route: cloud` to ask for a provider.
- Calibrate keeps a share of the client's requests to compare the local model with a provider. See [Calibration](#calibration).

Do these steps:

- TUI: select the client with `↑` and `↓`, and press `e`.
- Web console: open Change on the client's row, and select Save.
- API: send `PUT /system/router/clients/{name}`. This example lets the client use providers:

```bash
curl --cacert sibyl-ca.crt -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  -X PUT https://<address>:8443/system/router/clients/laptop -d '{"policy":"hybrid"}'
```

A `PUT` sets every field of the client. A field that the body leaves out goes back to its default. A `PUT` without `budget` removes the budget, and a `PUT` without `"calibrate": true` turns calibration off.

Make a client `hybrid` only when its prompts can leave the host.

### Make a new key or delete a client

A new key stops the old key at once. A deleted client's key also stops at once. Deleting the client is the way to revoke its key.

| Task | TUI | Web console | API |
|---|---|---|---|
| New key | `k` | Change, then New key for the client | `POST /system/router/clients/{name}/key` |
| Delete | `d` | Change, then Delete | `DELETE /system/router/clients/{name}` |

## Providers

A provider is a cloud service that serves models, such as Anthropic, OpenAI or OpenRouter. Each provider has a type, a base URL, a key and a list of models. Sibyl checks the key with the provider before it stores it.

| Type | Covers |
|---|---|
| `openai` | OpenAI, OpenRouter and other services with an OpenAI-compatible API |
| `anthropic` | The Anthropic API. It needs no base URL. |

### Add a provider in the TUI

1. Press `p`.
2. Pick the provider from the models.dev index. Sibyl keeps a copy of this index on the host and updates it daily.
3. Type the API key. Sibyl fills in the type and the base URL.
4. Choose the models to list. Sibyl shows each model with its context size and price.

To add a provider that the index does not list, pick "Other". Then type the type, the base URL, the key and the models yourself.

### Add a provider in the web console

1. Open Add or change a provider.
2. Fill in the name, the API, the base URL, the API key, and the models to list, separated by spaces.
3. Select Save.

To change a provider, give the same name. An empty API key keeps the stored key. A change keeps the provider's prices.

### Add a provider with the API

Give the fields yourself:

```bash
curl --cacert sibyl-ca.crt -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  -X PUT https://<address>:8443/system/router/providers/openrouter \
  -d '{"type":"openai","base_url":"https://openrouter.ai/api/v1","api_key":"<provider key>","models":["<model>"]}'
```

A `PUT` without `api_key` keeps the stored key.

### Delete a provider

Its key is deleted too. If a profile names the provider, change the profile first.

- TUI: press `x`.
- Web console: open Delete on the provider's row.
- API: `DELETE /system/router/providers/{name}`.

### Use a provider model

A `hybrid` client can ask for `<provider>/<model>`, such as `openrouter/<model>`, or for `auto`. A `local-only` client gets an error for a provider model.

## Prices and budgets

Sibyl prices each provider request from the usage that the provider returns. Prices are US dollars per million tokens. Sibyl looks for a price in this order:

1. The `prices` of the provider, which you set.
2. The models.dev index.
3. For the type `anthropic`, a built-in table of Anthropic prices.

Budgets therefore work for most providers without a price of your own. A model that none of these lists has no price. [Configuration](../reference/configuration.md) lists the price fields.

### Set a budget

A new client has no spending limit. A budget has a daily and a monthly limit in US dollars. A day and a month are UTC.

- TUI: press `e` on the client, and fill in Daily budget and Monthly budget. Empty means no limit.
- Web console: open Change on the client, and fill in the budget fields.
- API: add `"budget":{"daily_usd":5,"monthly_usd":50}` to the client's `PUT`.

When a budget is spent:

- An `auto` request runs on the profile's local model.
- A request for a provider model gets 429.

A client with a budget cannot use a provider model that has no price, because its cost cannot be counted. Such a request gets 429 when no other route is left. Requests that run at the same time can go over a budget by their own cost, because the cost is known only after a request.

The TUI and the web console show the spend of each client today and this month. A `hybrid` client with no budget is marked "no budget".

## Profiles and rules

A profile is a named set of rules. The model `auto` runs the rules of the `auto` profile. The model `auto:<name>` runs the rules of the profile `<name>`.

A profile has these parts:

| Part | Meaning |
|---|---|
| Local model | The model that the route `local` means |
| Rules | An ordered list. The first rule whose conditions all match decides the route. |
| Fallback | The routes to try, in order, when the chosen route fails |
| Local wait | The seconds to wait for a local model before the next route. The default is 30. With no next route, the router waits until the model loads. |

A rule has a name, conditions under `when`, and a route. The route is `local`, a local model ID, or `<provider>/<model>`. A rule with no conditions matches every request.

A rule that names a provider does not match a `local-only` client, so the next rule runs. A `local-only` client also skips every provider in the fallback list.

### Rule conditions

| Condition | Value | Matches when |
|---|---|---|
| `sensitive` | true or false | The request holds a private key, an API token, a credential pattern, or one of your sensitive words |
| `has_images`, `has_tools`, `json_schema`, `stream` | true or false | The request has images, tools, a JSON schema, or asks for streaming |
| `local_ready` | true or false | The local model is loaded and ready |
| `prompt_tokens_over` | A number, or `local_context` | The estimated prompt is longer than that, or than the local model's context |
| `max_tokens_over` | A number | The request asks for more output tokens than that |
| `task` | A list of task labels | The classifier gives one of these tasks |
| `difficulty` | A list of difficulty labels | The classifier gives one of these difficulties |
| `needs_provider` | true or false | Most of the nearest judged requests went better at the provider. See [Calibration](#calibration). |
| `hint` | A list of `local`, `cloud` or `auto` | The request's `X-Sibyl-Route` header has one of these values |
| `clients` | A list of client names | The request comes from one of these clients |

### Change profiles and rules in the TUI

- `m` makes, changes or deletes a profile: its local model, its fallback routes and its local wait.
- `r` adds, edits, moves or deletes a rule.

The rule form shows the common conditions. It keeps the conditions that it does not show.

### Change profiles and rules in the web console

The Profiles and rules section lists each profile with its rules.

- Change the profile edits the local model, the fallback and the local wait.
- New profile makes a profile.
- Add a rule adds a rule to that profile.
- Edit, Up, Down and Delete act on one rule.

### Change profiles and rules with the API

`PUT /system/router/profiles` takes the whole profiles file as JSON and replaces it. It holds the profiles, the sensitive words, the classifier settings and the calibration settings. To change one part, read the current file under `profiles` in `GET /system/router`, change it, and send it back. The System API checks every rule before it saves the file.

This profile, in the YAML form of the file, keeps secrets on the host and sends hard code and maths to a provider:

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

### Test a route without sending the request

`POST /system/router/explain` shows the route for a request without sending it. It takes `{"client", "format", "hint", "request"}`. The format is `chat`, the default, or `messages`. The answer shows the request's profile, each rule with match or no match, and the route.

## Route by task and difficulty

The classifier gives each request two labels, which the rules can test. The classifier runs on the host, so no prompt leaves the host to be labelled.

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

The first five are the `task` labels. The last three are the `difficulty` labels.

The classifier embeds the last user message with the local embedding model. The 5 most similar labelled examples vote. Sibyl ships 150 starter examples, 10 for each pair of labels. The classifier runs only for a profile that has a `task` or `difficulty` rule. When it fails or takes more than 300 ms, both labels are `unknown`, and the next rules decide.

### See the labels of a text

Send the text to the classifier. The answer shows the labels and the nearest examples.

```bash
curl --cacert sibyl-ca.crt -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  https://<address>:8443/system/router/classify -d '{"text":"Find the race in this Go code"}'
```

### Add your own examples

If the classifier labels your requests wrongly, add examples of them.

1. Write a JSON list of `{"text", "task", "difficulty"}`.
2. Send it with `PUT /system/router/examples`. It replaces your earlier list.

Your examples add to the starter examples. `GET /system/router/examples` returns your list.

### Set the sensitive words and the classifier

Sensitive words mark a request as `sensitive`. The classifier settings are the embedding model, the number of voting examples, the timeout, the prefix, and whether to use the starter examples.

- TUI: press `s`.
- Web console: the Sensitive words and classifier section.
- API: the `sensitive_words` and `classifier` parts of `PUT /system/router/profiles`.

The prefix goes before every text that the classifier embeds. The default is the instruction format of the Qwen3 embedding models. For another embedding model, set the prefix that its model card gives, or `""` for none. Only the API sets the prefix. [Configuration](../reference/configuration.md) lists the classifier fields and their defaults.

## Compare profiles with sibyl-eval

`sibyl-eval` compares profiles before you trust one. It sends a labelled set of requests to the classifier and to explain, so no request leaves the host. It prints the classifier's accuracy and, for each profile, the share of local requests and the estimated provider cost.

Run it from a clone of the Sibyl repository:

```bash
go run ./cmd/sibyl-eval -url https://<address>:8443 -api-key api.key -ca sibyl-ca.crt -profiles auto,<other profile>
```

The default set is `internal/router/heldout.jsonl`. It holds 60 requests, 4 for each pair of labels. None of them is in the starter examples.

| Flag | Meaning |
|---|---|
| `-url` | The host's API address, such as `https://192.168.1.26:8443` |
| `-api-key` | File with the System API key. Default `api.key`. |
| `-ca` | File with the host's CA certificate, when the system does not trust it |
| `-profiles` | Comma-separated profiles to compare. Default `auto`. |
| `-client` | The client whose policy explain uses. Default `default`. |
| `-set` | Another labelled request set |
| `-send` | Also send each request. Provider requests then cost money. |
| `-key` | File with an inference key, for `-send`. Default `inference.key`. |
| `-out` | A JSON lines file for the results and the answers |
| `-max-tokens` | With `-send`, the answer limit. Default 8192. |
| `-judge` | With `-send`, comma-separated models that score each answer from 1 to 10 |
| `-judge-tokens` | The judge's token limit. Default 4096. |
| `-rescore` | A results file from `-out`. Adds the `-judge` scores that it lacks, and writes `-out`. |
| `-retry` | With `-rescore`, sends the requests that got no answer again |

[Router design](../design/router.md) has the measured results on the test host.

## Calibration

Calibration measures the local model against a provider on your own requests. It turns the results into rule proposals.

### Turn on calibration for a client

Calibration works for `hybrid` clients only.

- TUI: press `e` on the client, and set Calibrate.
- Web console: open Change on the client, and check Calibrate.
- API: send `"calibrate": true` in `PUT /system/router/clients/{name}`.

The router then keeps 2% of that client's chat requests to a profile, with the answers, in `/var/lib/sibyl/config/router/calibration/`. These samples are the only message content that Sibyl keeps. The router never keeps a request that is sensitive, that has images or tools, or that comes from a `local-only` client.

### How a sample is judged

When no local request is in flight, the router gets the answer of the other side for each sample. A local answer gets an answer from the profile's first provider route. A provider answer gets one from the local model. The router does not load a local model for this.

A judge then compares the two answers in a random order, without the model names. By default, the judge is the provider model with the highest output price. You can choose `local` as the judge, but the local model favours its own answers.

Calibration has its own provider budget, 1 US dollar per UTC day by default. A provider model with no price cannot be counted against it. Calibration then waits until you add a price for that model in the provider's `prices`.

### Change the calibration settings

The settings are the sample rate, the daily budget, the judge, the provider to compare with, the keep time, the most samples kept, the measurement of new local models, and the automatic proposals.

- TUI: press `c`.
- Web console: the Calibration settings section.
- API: the `calibration` part of `PUT /system/router/profiles`.

```yaml
calibration:
  sample_rate: 0.02     # share of requests kept
  daily_usd: 1          # 0 stops all provider calls
  judge: openrouter/<model>   # default: the provider model with the highest output price
  compare: openrouter/<model> # default: the profile's first provider route
  keep_days: 14
  max_samples: 2000
```

### Read the results

The results show, for each local model, task and difficulty, how often the local answer won or tied. They also show the provider cost and the latency of each side.

- TUI: the Router screen shows the samples, the reason the worker waits, the rate per task and difficulty, and the proposals.
- Web console: the Calibration section of the Router page.
- API: `GET /system/router/calibration`.

### Apply a proposal

After 50 comparisons for a task and difficulty, the results propose a rule change when they disagree with the profile:

- A request that the profile sends to a provider is proposed for the local model when the local answer won or tied at least 90% of the time.
- A request that the profile keeps local is proposed for the provider when the local answer won or tied less than 60% of the time.

To apply a proposal:

- TUI: press `a`, pick the proposal, and confirm.
- Web console: select Apply next to the proposal.
- API: send its `id`:

```bash
curl --cacert sibyl-ca.crt -H "Authorization: Bearer $ADMIN" -H "Content-Type: application/json" \
  https://<address>:8443/system/router/calibration/apply -d '{"id":"<id>"}'
```

The new rule goes before the rules that test labels, so the rules for sensitive and long requests still come first.

To let Sibyl apply proposals by itself, set `auto_apply: true` in `calibration`. Sibyl then applies a proposal only when the profile already names its route, and it logs each change.

### Measure a new local model

When a profile gets a local model that has not been measured, the router measures it first with the 60 labelled requests of `sibyl-eval`. The router answers them with the local model when it is idle. It gets the provider answers and the verdicts under the same daily budget. To turn this off, set `no_seed: true` in `calibration`.

### Route on the calibration results

A rule can test the calibration results of each request with `needs_provider: true`. It matches a request when most of the nearest judged requests went better at the provider. Below 20 judged requests for the local model, the condition matches nothing.

### Delete the samples

This deletes every calibration sample, with the results that come from them.

- TUI: press `C`.
- Web console: open Delete every sample.
- API: `DELETE /system/router/calibration`.

## When a route fails

- A local model that is loading: the router waits up to the profile's local wait, then tries the fallback list.
- A provider 429 or 5xx: the router tries once more after the provider's `retry-after` delay or a short wait, then tries the next fallback.
- A streamed response falls back only before the first byte reaches the client. After that, the router ends the stream with an error chunk.
- The provider's own error text goes back to the client.

## What you can see

Each response has an `X-Sibyl-Route` header that names the route, such as `local/qwen3-32b-q4_k_m`. For a profile, it also names the profile and the rule. A client can send `X-Sibyl-Route: local` to keep one request on the host.

The router keeps a decision log in `/var/lib/sibyl/config/router/log/decisions.jsonl`. It has one line per request: the time, the client, the requested model, the route, the rule, the status, the token counts, the cost and the latency. It never records message content.

- TUI and web console: the Router screen shows the requests, the routes, the spend per client and the last decisions. It also counts the requests with no price, and the requests that a client asked again within a minute.
- API: `GET /system/router/stats` shows the requests per client and per route.
