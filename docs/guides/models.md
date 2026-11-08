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

## Load and unload a model

llama-swap loads a model when a request asks for it. A model that spans the GPUs runs alone, with the embedding models. Models pinned to different GPUs run together. The Status screen of `sibyl` and the Status page of the web console list the loaded models.

1. To load a model before a request needs it, select it on the Models screen and press `l`. In the web console, choose Load on the Models page. llama-swap first stops the models that cannot run with it.
2. To free its GPU memory, press `l` again, or choose Unload on the Models page or the Status page.

A large model takes a minute or more to load. The Status screen shows "loading" until it is ready. If the load fails, the Status screen shows the error.

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

- The types `draft-simple`, `draft-eagle3`, `draft-dflash` and `draft-dspark` need a draft model. The Draft model list shows the installed drafters, each with the type that it needs, such as `draft-dflash`. Tick that type too. `draft-simple` uses a plain installed model that is smaller than this one, so the list shows the plain models only when `draft-simple` is ticked.
- If a draft model is set but no type that loads it is ticked, the save fails and names the type to tick. The form stays open with your changes.
- `draft-mtp` uses the model's own multi-token prediction layer.
- The `ngram-*` types need no second model.

A model cannot be its own draft model. If you delete a draft model, Sibyl turns speculative decoding off for the models that used it and reports an issue. Those models keep running.

#### Find a drafter

A drafter is a small model that is trained to draft tokens for one model, such as a DFlash or EAGLE-3 drafter. It runs inside its model's llama-server, and it cannot answer requests by itself. Sibyl does not list it as a model that clients can call. The model list shows it under the model that uses it.

1. On the Models screen, select a model and press `f`. In the web console, choose Drafters on the Models page.
2. Sibyl searches Hugging Face for drafters of the model and of the models that it was made from. A drafter marked "for the base model" was trained for a model further up. It accepts fewer of its tokens on a finetune, and it can make the finetune slower.
3. Choose a drafter. If it is not installed, choose a file to download. Q8_0 is the default. When the download finishes, open the drafter list again.
4. Choose the installed drafter, and confirm the test. Sibyl runs two prompts without and with the drafter. The test unloads the running models and takes a few minutes.
5. Read the result. It gives the speed without and with the drafter, and how many drafted tokens the model kept. If the drafter makes the model faster, turn it on.

The drafter's weights use GPU memory, so the automatic context of its model gets shorter when you turn it on.

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
3. Read the result in the model's details on the Models screen. The TUI names a faster backend only when it generates at least 5% faster. A smaller gap is within the noise of one run, so it says that the backends are within 5% of each other.
4. If you want the faster backend, change it on the Hardware screen.

The backend applies to every model, because llama-swap runs one image.

## Optimize a model

The optimizer measures llama.cpp settings for one model on your hardware, and recommends the fastest that loads. It covers the backend, the split mode across GPUs, the KV cache type, the batch sizes, the MoE layers in system RAM and, for throughput, the number of parallel requests. [The optimizer design](../design/optimizer.md) describes each stage.

1. On the Models screen, select a model and press `o`. In the web console, open the model's page and use the Optimizer section.
2. Choose what to optimize for:
   - One user: generation speed, then prompt speed. Use this for one person or one agent.
   - Throughput: the total speed over parallel requests, where each request keeps at least half the speed of one alone.
3. Wait 5 to 15 minutes. Sibyl unloads the running models while the optimizer runs. Press `o` again to cancel.
4. Read the result in the model's details: the speed now, the best speed, and each setting that changes.
5. Press `a` to apply the settings. In the web console, use "Apply these settings". If a different backend was faster, Sibyl asks whether to switch the backend too. The backend applies to every model.

A setting wins only when it beats the settings now by more than 3 percent and by more than the noise of the runs. The optimizer applies nothing by itself. A result is for the hardware and the llama.cpp version of the run, so run it again after an update.

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
