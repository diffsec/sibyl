# llama.cpp (HIP/ROCm) + llama-swap, for the Radeon and Ryzen GPUs that ROCm
# supports. GPU_TARGETS must match hardware.HIPTargets, which a test checks.

ARG ROCM_VERSION=7.2.4
ARG ROCM_BUILD_IMAGE=docker.io/rocm/dev-ubuntu-24.04:${ROCM_VERSION}-complete
ARG RUNTIME_IMAGE=docker.io/ubuntu:24.04

FROM ${ROCM_BUILD_IMAGE} AS build

ARG LLAMA_CPP_REF=b11889
# RDNA2 (RX 6800/6900, PRO W6800), RDNA3 (RX 7000, PRO W7000), RDNA3.5
# (Ryzen AI 300 and Strix Halo APUs), RDNA4 (RX 9000, AI PRO R9700). The same
# list as llama.cpp's own ROCm image, without the Instinct cards.
ARG GPU_TARGETS="gfx1030;gfx1100;gfx1101;gfx1102;gfx1150;gfx1151;gfx1200;gfx1201"
ARG GGML_HIP_RCCL=OFF

RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential cmake git ca-certificates libssl-dev \
    && rm -rf /var/lib/apt/lists/*

RUN git clone --depth 1 --branch "${LLAMA_CPP_REF}" https://github.com/ggml-org/llama.cpp /src

WORKDIR /src
RUN HIPCXX="$(hipconfig -l)/clang" HIP_PATH="$(hipconfig -R)" \
    cmake -S . -B build \
        -DCMAKE_BUILD_TYPE=Release \
        -DGGML_HIP=ON \
        -DGPU_TARGETS="${GPU_TARGETS}" \
        -DGGML_HIP_RCCL="${GGML_HIP_RCCL}" \
        -DGGML_NATIVE=OFF \
        -DLLAMA_BUILD_TESTS=OFF \
        -DLLAMA_BUILD_EXAMPLES=OFF \
        -DLLAMA_BUILD_UI=OFF \
        -DLLAMA_USE_PREBUILT_UI=OFF \
    && cmake --build build --config Release -j"$(nproc)" --target llama-server llama-bench

# Collect the binaries, their shared libraries, and only the rocBLAS and
# hipBLASLt kernel files for GPU_TARGETS. The full ROCm tree is ~20 GB.
COPY collect-rocm-libs.sh /usr/local/bin/collect-rocm-libs.sh
RUN /usr/local/bin/collect-rocm-libs.sh /out "${GPU_TARGETS}" build/bin/llama-server build/bin/llama-bench

FROM ${RUNTIME_IMAGE} AS llama-swap-dl

ARG LLAMA_SWAP_VERSION=266
ARG LLAMA_SWAP_SHA256=
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates curl \
    && rm -rf /var/lib/apt/lists/*
COPY fetch-llama-swap.sh /usr/local/bin/fetch-llama-swap.sh
RUN /usr/local/bin/fetch-llama-swap.sh "${LLAMA_SWAP_VERSION}" "${LLAMA_SWAP_SHA256}" /out

FROM ${RUNTIME_IMAGE}

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates libgomp1 libssl3t64 curl \
        libdrm2 libdrm-amdgpu1 libelf1t64 libnuma1 \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --system --gid 10001 app \
    && useradd --system --uid 10001 --gid 10001 --home /app --no-create-home app

COPY --from=build /out/ /
COPY --from=llama-swap-dl /out/llama-swap /app/llama-swap
COPY config.yaml /app/config/config.yaml
COPY check-libs.sh /usr/local/bin/check-libs.sh
# Fail the build if any binary or library has an unresolved dependency.
RUN LD_LIBRARY_PATH=/opt/rocm/lib:/opt/llama/lib /usr/local/bin/check-libs.sh /opt/llama/bin /opt/llama/lib /opt/rocm/lib

ENV PATH=/app:/opt/llama/bin:${PATH} \
    LD_LIBRARY_PATH=/opt/rocm/lib:/opt/llama/lib \
    ROCBLAS_TENSILE_LIBPATH=/opt/rocm/lib/rocblas/library \
    HIPBLASLT_TENSILE_LIBPATH=/opt/rocm/lib/hipblaslt/library \
    SIBYL_GPU_BACKEND=ROCm

USER 10001:10001
WORKDIR /app
EXPOSE 8080
HEALTHCHECK CMD ["curl", "-fsS", "http://127.0.0.1:8080/health"]
ENTRYPOINT ["/app/llama-swap", "-listen", ":8080", "-config", "/app/config/config.yaml", "-watch-config"]
