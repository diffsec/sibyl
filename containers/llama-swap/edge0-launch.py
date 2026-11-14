#!/usr/bin/env python3
"""Start edge0-strata's server for one Edge0 model folder.

llama-swap runs this as the command of an Edge0 model. The folder's
model.json is a complete server configuration for an NVIDIA start from inside
the folder. This script changes what differs here, writes the result to a
work directory, and replaces itself with the server.
"""
import argparse
import json
import os
import sys

ENGINE = "/opt/llama/bin/strata"
SERVER = "/opt/edge0/serve/server.py"
ROCM_LIB = "/opt/rocm/lib"
# Options whose value differs here, with the number of values each one takes.
DROP = {"--pool-workers": 1, "--max-context": 1, "--vram-reserve-later-mib": 1}
# A card that another model also uses can hold less than the engine planned
# for. On the test host a later card with 240 MiB free stalled the first
# request, and 1400 MiB of reserve did not.
RESERVE_LATER_MIB = "1400"


def absolute(value, model_dir):
    """Resolve a "./" path of the folder's configuration against the folder."""
    if value == ".":
        return model_dir
    if isinstance(value, str) and value.startswith("./"):
        return os.path.join(model_dir, value[2:])
    return value


# On one GPU the engine keeps the expert file in system RAM, plus about
# RAM_OVERHEAD. On a layer split it keeps only the experts that no GPU holds
# (--resident-experts). On two 24 GB cards that was 21.4 GiB of a 37.3 GiB
# file, which is 57%, so SPAN_SHARE leaves a margin. The same figures are in
# internal/models (Edge0RAM).
RAM_OVERHEAD = 6 << 30
SPAN_SHARE = 0.65


def ram_short(experts_path, span=False, meminfo="/proc/meminfo"):
    """Return (needed, available) in bytes when RAM is short, else None."""
    try:
        experts = os.path.getsize(experts_path)
        need = (int(experts * SPAN_SHARE) if span else experts) + RAM_OVERHEAD
        with open(meminfo, encoding="ascii") as f:
            for line in f:
                if line.startswith("MemAvailable:"):
                    avail = int(line.split()[1]) << 10
                    return (need, avail) if avail < need else None
    except (OSError, ValueError, IndexError):
        pass
    return None  # the size or the memory figure cannot be read


def configure(manifest, model_dir, alias, gpus, context, work):
    """Return the server configuration for this container."""
    args, src, i = [], manifest["args"], 0
    while i < len(src):
        arg = src[i]
        # The engine picks the worker count from the CPU when the option is absent.
        if arg in DROP:
            i += 1 + DROP[arg]
            continue
        # --pcie-mode dma is not tested on a layer split with the engine's
        # split fix. Before the fix it stopped the first request.
        if arg == "--pcie-mode" and len(gpus) > 1:
            i += 2
            continue
        args.append(absolute(arg, model_dir))
        i += 1
    args += ["--max-context", str(context)]
    if len(gpus) > 1:
        args += ["--vram-reserve-later-mib", RESERVE_LATER_MIB]
        # Two cards hold most of the experts, so RAM keeps only the rest.
        # Without this the engine keeps the whole expert file in RAM too.
        if "--resident-experts" not in args:
            args.append("--resident-experts")

    env = {k: absolute(v, model_dir) for k, v in manifest.get("env", {}).items()}
    return {
        "model_name": alias,
        "backend": "hip",
        "exe": ENGINE,
        "args": args,
        "cwd": work,
        "log": os.path.join(work, "engine.log"),
        "tokenizer": absolute(manifest.get("tokenizer", "./tokenizer"), model_dir),
        "gpus_asked": True,
        "gpu": gpus if len(gpus) > 1 else gpus[0],
        "lib_dirs": [ROCM_LIB],
        "env": env,
    }


def main():
    ap = argparse.ArgumentParser(description=__doc__.splitlines()[0])
    ap.add_argument("--model-dir", required=True)
    ap.add_argument("--alias", required=True, help="the model name that the server answers to")
    ap.add_argument("--gpus", required=True, help="GPU numbers as HIP numbers them here, such as 0 or 0,1")
    ap.add_argument("--context", required=True, type=int)
    ap.add_argument("--port", required=True, type=int)
    ap.add_argument("--work", default="/tmp/edge0", help="a writable directory for the configuration and the engine log")
    ap.add_argument("--print-config", action="store_true", help="print the configuration and stop")
    opt = ap.parse_args()

    gpus = [int(g) for g in opt.gpus.split(",")]
    with open(os.path.join(opt.model_dir, "model.json"), encoding="utf-8") as f:
        manifest = json.load(f)
    work = os.path.join(opt.work, opt.alias)
    cfg = configure(manifest, opt.model_dir, opt.alias, gpus, opt.context, work)
    if opt.print_config:
        json.dump(cfg, sys.stdout, indent=1)
        return
    # Without the RAM, the engine pushes the host into swap or the kernel
    # stops a process. Refuse the load with a message that says why.
    short = ram_short(cfg["env"].get("STRATA_LATENT_A", os.path.join(opt.model_dir, "experts.bin")), span=len(gpus) > 1)
    if short:
        sys.exit("edge0: %s keeps %.1f GiB of system RAM, and %.1f GiB is available"
                 % (opt.alias, short[0] / (1 << 30), short[1] / (1 << 30)))
    # The image sets GPU_MAX_HW_QUEUES=1 for idle llama-server processes. This
    # engine waits on the GPU in one stream for a copy in another stream. On
    # one hardware queue the copy never starts, and the first request stops
    # with "verify: timed out at layer N".
    os.environ.pop("GPU_MAX_HW_QUEUES", None)
    os.makedirs(work, exist_ok=True)
    path = os.path.join(work, "config.json")
    with open(path, "w", encoding="utf-8") as f:
        json.dump(cfg, f, indent=1)
    os.execv(sys.executable, [sys.executable, SERVER, "--engine", "strata", "--config", path,
                              "--host", "127.0.0.1", "--port", str(opt.port), "--no-api-key"])


if __name__ == "__main__":
    main()
