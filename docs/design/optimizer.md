# Inference optimizer design

This document describes the inference optimizer. It finds the llama.cpp settings that run a model fastest on the host's own hardware, by measurement. An upstream LLM can suggest more settings to try, but only a measurement decides. [The design overview](overview.md) holds the rest of the design.

## Goals

- Find faster settings for one model on this host, with numbers that the owner can check.
- Never apply settings that were not measured, and never apply settings that do not load at the model's real context size.
- Keep the owner in control: the optimizer recommends, and the owner applies with one action.
- Send no prompts, no user data and no model files off the box. The optional advisor gets hardware facts, model facts and benchmark numbers only.

## What already exists

- `internal/models/fit.go` chooses the context size and the KV cache types from the free VRAM.
- `POST /models/{id}/benchmark` runs `llama-bench` once in each backend image through the host agent (`os/rootfs/usr/libexec/sibyl/agent`, `bench`), and recommends the backend with the fastest generation.
- `models.Meta` reads the layer count, the expert count and the expert tensor sizes from the GGUF file.

The optimizer replaces the fixed benchmark with a search over more settings, and it keeps the same path to the host.

## Profiles

The owner picks a profile for each run.

| Profile | Score | Tool |
|---|---|---|
| `single` | Generation tokens per second at a context depth, then prompt tokens per second | `llama-bench` |
| `throughput` | Total tokens per second over parallel requests, with a floor on the speed of each request | `llama-batched-bench` |

The context depth of the `single` profile is the number of tokens that are already in the KV cache when the test runs. It is 8,192 tokens, or half the model's context if that is smaller. An empty cache makes generation look faster than a real chat. The floor of the `throughput` profile is half of the single-request generation speed, so a high total with slow answers does not win.

## Settings that the search covers

| Setting | llama.cpp flag | Model setting | Note |
|---|---|---|---|
| Batch and micro-batch | `-b`, `-ub` | `batch`, `ubatch` (new) | Large effect on prompt speed. A larger micro-batch needs a larger compute buffer. |
| KV cache type | `-ctk`, `-ctv` | `kv_cache` | `q8_0` and `q4_0` free VRAM for context, and lower quality a little. |
| MoE experts in system RAM | `-ncmoe`, `-ot` | `moe.cpu_layers`, `args` | Only for a model whose experts do not fit in VRAM. |
| Split mode | `-sm` | `split_mode` (new) | `layer` or `row`, only with more than one GPU. |
| Tensor split | `-ts` | `tensor_split` | Only with more than one GPU. |
| Parallel slots | not in `llama-bench` | `parallel` | The `throughput` profile only. |
| Backend | the image | the hardware configuration | The backend applies to every model, so the optimizer reports it apart, and applying it restarts llama-swap. |

Flash attention stays on, and all layers stay on the GPUs, as llama-swap runs them today. Threads matter only for layers on the CPU, so the search leaves them alone.

## The search

The search is a fixed sequence of stages. Each stage runs one tool once. `llama-bench` takes a comma list for each flag and tests every combination, so one run covers one stage.

1. Baseline: the model's current settings.
2. Backend: each backend that can run on the selected GPUs. With more than one GPU, split mode `layer` and `row`.
3. KV cache: `f16` and `q8_0`. A cache type that does not fit the model's context in VRAM (`FitContext`) is left out.
4. Batch: micro-batch 256, 512, 1024 and 2048, with batch 2048 and 4096, where the batch is at least the micro-batch.
5. MoE: for a model whose experts do not fit in VRAM, the smallest `cpu_layers` value that fits, and two values above it.
6. Advisor: the candidates that the advisor proposes, if the owner turned it on.
7. Parallel: for the `throughput` profile, 1, 2, 4 and 8 parallel sequences at the model's context.
8. Verify: the host agent starts `llama-server` with the best settings at the model's real context size, waits for `/health`, and stops it.

Each stage keeps the best result so far and passes it to the next stage. A result wins only when it beats the current best by more than three standard deviations of the two runs, or by 3 percent, whichever is larger. `llama-bench` reports the standard deviation (`stddev_ts`). Noise therefore does not change a setting.

`llama-bench` allocates a context only for the test, not for the model's full context. A micro-batch that fits in the test can still fail at the real context. The verify stage catches that case. If the verify fails, the optimizer drops the last changed setting and verifies again.

## The path to the host

The System API runs in a container and has no access to Podman. The optimizer uses the host agent, as the benchmark does.

- A new agent request, `tune.<model>`, runs one tool from `$RUN/tune/<model>.*` files: the tool (`bench`, `batched` or `server`), the backend, the model path, the GPUs and the arguments.
- The agent checks every argument against an allowlist of flags. Each flag has a pattern for its value. An argument that is not on the list fails the request. The model path, the pool and the GPUs get the same checks as the benchmark.
- The tool runs in the backend image with no network, all capabilities dropped and `no-new-privileges`, as the benchmark does.
- The images build `llama-batched-bench` as well as `llama-server` and `llama-bench`.

The search itself runs in the System API (`internal/optimize`). It writes one request for each stage, and reads the result files.

## The advisor

The advisor is off by default. When the owner turns it on, the optimizer asks one provider model that the router already has.

- A new router route, `POST /sibyl/advise`, takes a provider and a model. Only the System API key can call it, as with `/sibyl/classify`. The provider keys stay in the router. The router counts the cost in `usage.json` under `@optimizer`, apart from the calibration budget.
- The optimizer sends the GPU names and VRAM, the system RAM, the backend, the model architecture, the parameter count, the quantization, the layer and expert counts, the context size, and every measured result.
- The advisor answers with JSON: up to six candidates, each one a set of the settings in the table above. The optimizer checks each candidate with the same allowlist as the agent, and drops what fails.
- The advisor is most useful for `-ot` patterns on a new MoE architecture, and for the order of the stages on hardware that the fixed search does not know.

## API

| Route | Purpose |
|---|---|
| `POST /models/{id}/optimize` | Start a run: `{"profile": "single" or "throughput", "advisor": {"provider": "...", "model": "..."}}`. The advisor is optional. |
| `GET /models/{id}/optimize` | The state: the stage, every measured result, and the recommendation when the run is done. |
| `DELETE /models/{id}/optimize` | Cancel the run. |
| `POST /models/{id}/optimize/apply` | Write the recommended model settings, and reload llama-swap. |

One run goes at a time on the host, because each run unloads the models and uses every selected GPU.

## Limits

- A run unloads the running models, so inference stops for its length: about 5 to 15 minutes for one model.
- `llama-bench` measures the model alone. It does not measure speculative decoding, so a model with a draft model keeps its speculative settings, and the optimizer measures without them.
- A recommendation is for the hardware and the llama.cpp version of the run. An update of either one makes the result old, and the console says so.

## Phases

1. Model settings `batch`, `ubatch` and `split_mode`. The `tune` agent request and its allowlist. `llama-batched-bench` in the images.
2. `internal/optimize`, the API routes, and the verify stage.
3. The TUI and the console.
4. The advisor and `/sibyl/advise`.
