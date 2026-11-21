# llama.cpp patches

The HIP and Vulkan images apply each `.patch` file in this directory to llama.cpp at `LLAMA_CPP_REF`. The NVIDIA image takes llama.cpp's own CUDA build, so it does not have them.

Keep a patch only while llama.cpp does not have the fix. When you move `LLAMA_CPP_REF` to a release that has it, delete the file. `git apply` fails the build if a patch does not apply.

| File | Source | What it fixes |
|---|---|---|
| `0001-inkling2-loop-reshare-scales.patch` | [ggml-org/llama.cpp#32704](https://github.com/ggml-org/llama.cpp/pull/32704), commit `2a28df0f9bd55c4975b6bffb35229a6020720f13` | The second and third pass through the looped layers of `inkling2_loop` ran the NVFP4 experts without their scales. The Unsloth files for Inkling 2 Mini have such scales, and gave wrong output. |
