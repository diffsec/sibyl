# Models

This guide shows you how to download models, change their settings, compare the backends, and add disks for models. Sibyl runs GGUF models with llama.cpp. llama-swap (the model loader) loads a model when a request asks for it, and unloads models when the GPUs need room for another.

You can manage models in two places:

- The web console: the Models page and the Downloads page.
- The `sibyl` TUI (the text console): the Models screen (`3`) and the Download screen (`4`).

[Consoles](consoles.md) describes both. The steps below give the TUI keys. The web console has a button or a form for each action.

## Download a model

1. Open the Download screen (`4`) and press `/`. In the web console, open the Downloads page.
2. Type a search, such as `qwen3 gguf`, and press Enter.
3. If you know the full name, such as `Qwen/Qwen3-Embedding-0.6B-GGUF`, paste it instead to open that repository.
4. Choose a repository.
5. Choose a variant. A variant is one quantization of the model, such as `Q4_K_M` or `Q8_0`. Smaller quantizations need less memory and lose some quality. The fit column tells you how the variant suits your GPUs (see the table below).
6. If the host has model pools, choose where the model goes.
7. Confirm.

| Fit | Meaning |
|---|---|
| `gpu` | The file fits in the VRAM of the selected GPUs, with room for the context. |
| `split` | The model needs system RAM as well, and it runs slower. |
| `no` | The model is too large for this host. |
| `?` | The GPU does not report its VRAM. This is normal on NVIDIA and Intel. |

The fit estimate compares the file size with the VRAM of the selected GPUs, less 2 GiB per GPU for the context. Some `split` models still run fully on the GPUs, because llama.cpp keeps a large token embedding table in system RAM. The model card says so.

The download runs in the background, and you can leave the screen. Sibyl does these things for each download:

- It checks that the disk has room for the rest of the download plus 2 GiB.
- It takes every file from one commit of the repository, even if the repository changes during the download.
- It checks each file against the SHA-256 checksum from Hugging Face, and deletes a file that does not match.
- It resumes a download that stops, and it resumes downloads after a restart.

The model appears on the Models screen when the download is complete. To retry a failed download, press `R`. To cancel a download, press `c`.

## Download a gated or private model

A gated model needs a Hugging Face account that accepted the model's terms. A private model needs an account that can read it.

1. On huggingface.co, accept the model's terms with your account.
2. Create an access token with read access at [huggingface.co/settings/tokens](https://huggingface.co/settings/tokens).
3. On the Download screen, press `t` and paste the token. In the web console, paste it under "Hugging Face token" on the Downloads page.

Sibyl checks the token with Hugging Face and shows the account name. Searches and downloads use the token at once, with no restart. In the web console, "Remove the Hugging Face token" signs out.

If Hugging Face refuses a request, the error says so, and the download stops at once. After you fix the cause, press `R` to resume the download.

## Change a model's settings

1. On the Models screen, select a model and press Enter. In the web console, open the model from the Models page.
2. Change the settings.
3. Save. The settings apply as soon as you save them.

The table lists the settings in the form. [Configuration](../reference/configuration.md#model-settings-modelsyaml) lists every field with its type and its allowed values.

| Setting | Effect |
|---|---|
| Layout | Span the model across all selected GPUs, or pin it to one GPU. |
| When VRAM is short | What an automatic context gives up. Quality (the default) keeps the KV cache at f16 and shortens the context. Context quantizes the KV cache only as far as it must, to `q8_0` and then `q4_0`, to keep the native context. |
| Context size | The longest conversation in tokens. See [Context size](#context-size). |
| Embedding | Marks a model for embeddings. Open WebUI uses the first embedding model for document search. |
| Hidden | Leaves the model out of the model lists. |
| Preset | A sampling preset for the model's main use: `general`, `coding`, `agentic` or `writing`. See [Sampling presets](#sampling-presets). |
| Sampling | Default temperature, top-k, top-p, min-p and penalties. An empty field shows in grey the value that applies: the preset's, the model file's recommendation, or llama.cpp's default. A value that you enter wins over the preset. A client can still override the values in each request. |
| Reasoning | The default reasoning of the model. See [Reasoning](#reasoning). |
| Parallel requests | The number of requests that the model answers at once. The requests share the context, so each one is sure of only the context size divided by this number. Empty leaves llama.cpp's default. More requests at once serve more programs, and each one gets a shorter context. |
| Tensor split | For a model that spans GPUs, the share of its layers on each GPU, such as `3,2`. Empty lets Sibyl split by the memory that each GPU has left after the embedding models. |
| KV cache | The precision of the context memory. `q8_0` halves its size with little loss. A type that you choose here is kept, and an automatic context fits around it. |
| Speculative decoding | Speeds up generation with a small draft model or with n-gram lookup. |
| Experts in system RAM | Mixture of Experts models only. See [Mixture of Experts models](#mixture-of-experts-models). |
| Expert GPU cache | Mixture of Experts models only. See [Mixture of Experts models](#mixture-of-experts-models). |

### Context size

If you leave the context size empty, Sibyl chooses it:

- Sibyl takes the native context of the model if it fits on the model's GPUs.
- Otherwise, Sibyl takes the largest multiple of 4,096 tokens that fits in 92% of their VRAM, less the memory of the embedding models on them.
- An embedding model with no context size gets 8,192 tokens.

The field shows what Sibyl chooses and its memory estimate. A number that you enter is kept even when it does not fit. The field then says that llama.cpp keeps part of the model in system RAM, which is slower. Sibyl chooses again after a GPU change, because it saves only the values that you enter.

### Speculative decoding

Speculative decoding guesses the next tokens cheaply and lets the model check them. You choose one or more types, which Sibyl tries in order.

- The types `draft-simple`, `draft-eagle3`, `draft-dflash` and `draft-dspark` need a draft model. A draft model is another installed model, smaller than this one.
- `draft-mtp` uses the model's own multi-token prediction layer.
- The `ngram-*` types need no second model.

A model cannot be its own draft model. If you delete a draft model, Sibyl turns speculative decoding off for the models that used it and reports an issue. Those models keep running.

### Mixture of Experts models

A Mixture of Experts (MoE) model sends each token through a few of its experts, so most expert weights are idle at any time. The form shows the Mixture of Experts settings only for a MoE model, and its description gives the size of the experts that stay in system RAM.

- Experts in system RAM keeps the expert weights of the first N layers, or `all` of them, in system RAM. The GPUs then hold the rest of the model and a longer context, and the CPU computes the experts.
- Expert GPU cache is GPU memory, in MiB, that keeps the most used experts so that the GPUs compute them. It needs experts in system RAM. The automatic context counts the cache, so a larger cache gives a shorter context.

A model larger than the RAM still runs. The kernel reads unused experts back from disk when a token needs them, which is slow. If you set `--load-mode` to `mlock` or `none` in the extra arguments, every expert stays in RAM instead.

## Reasoning

The Reasoning setting sets how much a reasoning model thinks before it answers, when a request does not say.

Model families use different names and values for this:

- gpt-oss reads `reasoning_effort` with levels such as `low` and `high`.
- Qwen3 reads `enable_thinking` as on or off.
- DeepSeek reads `thinking` as on or off.

Sibyl reads the chat template in the GGUF file to find the name and the values. The form then offers these choices:

- "Template default", which leaves the template's own default.
- The values that the template compares the variable with, such as `xhigh`. Sibyl also finds them when the template copies the variable into another name first.
- If the template reads both levels and a switch, the levels and `off`. For example, Qwen3.8 reads `reasoning_effort` and `enable_thinking`.
- A text field, if the template lists no values.

The field is hidden when the template has no reasoning control that Sibyl knows. The System API refuses a value that the template does not take.

Sibyl passes the value to llama-server as `--chat-template-kwargs`. A request's own `reasoning_effort` or `chat_template_kwargs` wins over the setting.

## Sampling presets

A preset sets the sampling values for one use of a model. It starts from the model's recommended sampling:

1. The values in the model file (`general.sampling`).
2. If the file has none, the model family's published values (Qwen, Gemma, Llama, DeepSeek, Mistral, gpt-oss).
3. If the family is not known, llama.cpp's defaults.

The preset then adjusts them for a use:

| Preset | Change |
|---|---|
| `general` | None: the recommendation as it is. |
| `coding` | Temperature at most 0.6, so code is more consistent. |
| `agentic` | Temperature at most 0.6, so tool calls are more consistent. |
| `writing` | Temperature at least 0.8, and min-p at least 0.05, which keeps a higher temperature coherent. |

gpt-oss keeps OpenAI's one setting, temperature 1.0 and top-p 1.0, for every preset. These adjustments are Sibyl's own defaults, not the model vendors'.

### Pick a preset in a request

A client can pick a preset for one request without a model reload. Ask for the model name `<model>:<preset>`, such as `qwen3.8-flash-next-reap-288-fn-r1:coding`. The model lists show each model with each preset. The preset fills only the fields that the request leaves out. [API clients](api-clients.md) shows how to call the APIs.

## Benchmark the backends

A backend is the GPU library that llama.cpp uses: `hip` (ROCm), `cuda` or `vulkan`. The benchmark tells you which backend of your image is faster on one model.

1. On the Models screen, select a model and press `b`. In the web console, choose Benchmark on the Models page.
2. Wait a few minutes. Sibyl unloads the running models and measures each backend of your variant on that model.
3. Read the result and the recommendation on the Models screen.
4. If you want the faster backend, change it on the Hardware screen.

The backend applies to every model, because llama-swap runs one image.

## Model pools

A model pool is an extra disk for models. Sibyl encrypts it and mounts it under `/var/lib/sibyl-pools`. The disk must be at least 16 GiB. It cannot be the system disk, or a disk that is mounted or in use.

### Make a pool

1. On the Host screen, press `p`. In the web console, open the Host page and use "Make a pool from a disk".
2. Choose the disk. The disk is erased.
3. Type the disk's serial number to confirm.
4. Write down the recovery key that Sibyl shows. Sibyl shows it once. You need it to move the pool to another computer.

Downloads can then go to the pool. If a model with the same ID is in two places, Sibyl uses the copy in the models folder first, then the copy in the lowest pool.

### Detach a pool

1. On the Host screen, select the pool and press `x`. In the web console, use Detach on the Host page.

llama-swap unloads its models first. The data and the key file stay on the disk.

## Related

- [Configuration](../reference/configuration.md): every model setting field and its allowed values.
- [System API](../reference/system-api.md): the routes for models, downloads and pools.
- [Design](../design/overview.md): how the downloads, the context fit and the MoE offload work.
