#!/usr/bin/env python3
"""Run real-block bench with ParallelTxTiming and summarize Amdahl headroom."""

from __future__ import annotations

import json
import math
import os
import shutil
import subprocess
import sys
import tempfile
from collections import Counter
from pathlib import Path

# ---------------------------------------------------------------------------
# CONFIG
# ---------------------------------------------------------------------------

os.environ["BENCHMARK_MODE"] = "both"
os.environ["GOMAXPROCS"] = "8"
os.environ["PARALLEL_TX_TIMING"] = "1"

REPO_ROOT = Path(__file__).resolve().parent
BENCHMARK_JSON_INPUT_DIR = REPO_ROOT / "benchmarks" / "blocks" / "blocksdata"
JSON_FILE = "25603091.json"
TIMING_JSONL = REPO_ROOT / "benchmarks" / "results" / "amdahl_timing.jsonl"
BENCH_LOG = REPO_ROOT / "benchmarks" / "results" / "_temp_amdahl_bench.log"
WAVE_EFFICIENCY_PLOT = REPO_ROOT / "benchmarks" / "results" / "wave_efficiency.png"
BRANCH_NAME = "local"
BENCHMARK_TIMEOUT = "30m"
RUNS = 100

# ---------------------------------------------------------------------------


def makespan_lb(costs: list[int], workers: int) -> int:
    """Lower bound on wave wall with W workers (equal-slot + longest-task)."""
    if not costs:
        return 0
    w = max(1, workers)
    return max(max(costs), (sum(costs) + w - 1) // w)


def mean(xs: list[float]) -> float:
    return sum(xs) / len(xs) if xs else 0.0


def load_samples(path: Path) -> list[dict]:
    samples: list[dict] = []
    with path.open(encoding="utf-8") as f:
        for line in f:
            line = line.strip()
            if not line:
                continue
            samples.append(json.loads(line))
    return samples


def wave_efficiency(wave: dict, avg_tx_ns: list[int], workers: int) -> float | None:
    """E = sum(seq tx costs) / (concurrent_wall * min(W, wave_size)).

    Drops waves with no concurrent_wall (direct-exec / sequential-in-wave) so we
    don't mix serial wall into a parallel-efficiency metric.
    """
    size = int(wave["size"])
    if size <= 0:
        return None
    costs = [avg_tx_ns[i] for i in wave["txs"]]
    serial_sum = sum(costs)
    wall = int(wave.get("concurrent_wall_ns") or 0)
    if wall <= 0 or serial_sum <= 0:
        return None
    denom_workers = min(max(1, workers), size)
    return serial_sum / (wall * denom_workers)


def plot_wave_efficiency(
    sizes: list[int],
    efficiencies: list[float],
    workers: int,
    out_path: Path,
) -> None:
    try:
        import matplotlib.pyplot as plt
    except ImportError:
        print("matplotlib not installed; skipping wave efficiency plot", file=sys.stderr)
        return

    # Mean efficiency per wave size for a guide line.
    by_size: dict[int, list[float]] = {}
    for s, e in zip(sizes, efficiencies):
        by_size.setdefault(s, []).append(e)
    mean_sizes = sorted(by_size)
    mean_effs = [mean(by_size[s]) for s in mean_sizes]

    fig, ax = plt.subplots(figsize=(8, 4.5))
    ax.scatter(sizes, efficiencies, alpha=0.35, s=28, label="per-wave samples")
    ax.plot(mean_sizes, mean_effs, color="C1", marker="o", linewidth=1.5, label="mean by size")
    ax.axhline(1.0, color="gray", linestyle="--", linewidth=1, label="ideal E=1")
    ax.set_xlabel("wave size")
    ax.set_ylabel(r"efficiency $E = \sum t_i^{seq} / (T_{concurrent} \cdot \min(W, |wave|))$")
    ax.set_title(f"Per-wave parallel efficiency (W={workers}; concurrent_wall>0 only)")
    ax.set_ylim(bottom=0)
    ax.grid(True, alpha=0.3)
    ax.legend()
    fig.tight_layout()
    out_path.parent.mkdir(parents=True, exist_ok=True)
    fig.savefig(out_path, dpi=150)
    plt.close(fig)
    print(f"wave efficiency plot -> {out_path}")


def analyze(samples: list[dict], plot_path: Path = WAVE_EFFICIENCY_PLOT) -> None:
    seq = [s for s in samples if s.get("mode") == "sequential"]
    par = [s for s in samples if s.get("mode") == "parallel"]
    if not seq or not par:
        print("Need both sequential and parallel samples in JSONL.", file=sys.stderr)
        sys.exit(1)

    n = min(len(seq), len(par))
    seq, par = seq[:n], par[:n]

    def avg_field(xs: list[dict], key: str) -> float:
        return mean([float(s.get(key, 0)) for s in xs])

    seq_incomplete = sum(1 for s in seq if not s.get("complete", True))
    par_incomplete = sum(1 for s in par if not s.get("complete", True))

    # Shared post-Process work (final/validate/write) is path-symmetric if validate
    # would pass. When parallel dies on gas/receipt/state mismatch, impute those
    # walls from sequential so par serial is not artificially low.
    shared_keys = ("finalization_ns", "validate_ns", "write_ns")
    seq_shared_parts = {k: avg_field(seq, k) for k in shared_keys}
    imputed = par_incomplete > 0
    if imputed:
        print(
            f"NOTE: imputing seq final/validate/write into par serial "
            f"(incomplete par inserts {par_incomplete}/{n}; seq incomplete {seq_incomplete}/{n}).\n"
            "  Assumption: had parallel validate passed, shared post-Process cost ≈ sequential.\n"
        )

    work_seq = avg_field(seq, "parallelizable_ns")
    work_par = avg_field(par, "parallelizable_ns")

    tax_seq = avg_field(seq, "grouping_ns")  # merge is 0 on seq
    tax_par = avg_field(par, "grouping_ns") + avg_field(par, "merge_ns")
    parallel_tax = tax_par - tax_seq

    shared_seq = sum(seq_shared_parts.values())
    if imputed:
        shared_par = shared_seq
        par_final = seq_shared_parts["finalization_ns"]
        par_validate = seq_shared_parts["validate_ns"]
        par_write = seq_shared_parts["write_ns"]
    else:
        par_final = avg_field(par, "finalization_ns")
        par_validate = avg_field(par, "validate_ns")
        par_write = avg_field(par, "write_ns")
        shared_par = par_final + par_validate + par_write

    serial_seq = tax_seq + shared_seq
    serial_par = tax_par + shared_par

    # Average per-tx costs from sequential runs (nanoseconds).
    tx_count = seq[0]["tx_count"]
    avg_tx = [0.0] * tx_count
    for s in seq:
        for i, v in enumerate(s["tx_apply_ns"]):
            avg_tx[i] += v
    avg_tx_ns = [int(round(v / len(seq))) for v in avg_tx]

    workers = max(1, int(mean([p["workers"] for p in par])) or int(os.environ.get("GOMAXPROCS", "1")))

    # Wave histogram + ideal makespan + per-wave efficiency.
    hist: Counter[int] = Counter()
    ideal_makespans: list[float] = []
    eff_sizes: list[int] = []
    eff_values: list[float] = []
    for p in par:
        for wave in p["waves"]:
            hist[int(wave["size"])] += 1
            eff = wave_efficiency(wave, avg_tx_ns, workers)
            if eff is not None:
                eff_sizes.append(int(wave["size"]))
                eff_values.append(eff)
        ideal = 0
        for wave in p["waves"]:
            costs = [avg_tx_ns[i] for i in wave["txs"]]
            ideal += makespan_lb(costs, workers)
        ideal_makespans.append(float(ideal))
    ideal_work = mean(ideal_makespans)

    t_seq = serial_seq + work_seq
    t_par = serial_par + work_par
    achieved = t_seq / t_par if t_par else float("nan")
    s_work = work_seq / ideal_work if ideal_work else float("nan")
    t_ideal = serial_par + ideal_work
    theoretical = t_seq / t_ideal if t_ideal else float("nan")
    t_no_tax = shared_par + ideal_work
    theoretical_no_tax = t_seq / t_no_tax if t_no_tax else float("nan")

    print(f"samples paired: {n}  txs: {tx_count}  workers: {workers}")
    print()
    print("=== serial breakdown (avg ms) ===")
    print(
        f"  seq  grouping={avg_field(seq,'grouping_ns')/1e6:.3f}  merge={avg_field(seq,'merge_ns')/1e6:.3f}  "
        f"final={seq_shared_parts['finalization_ns']/1e6:.3f}  validate={seq_shared_parts['validate_ns']/1e6:.3f}  "
        f"write={seq_shared_parts['write_ns']/1e6:.3f}"
    )
    imputed_tag = " [imputed from seq]" if imputed else ""
    print(
        f"  par  grouping={avg_field(par,'grouping_ns')/1e6:.3f}  merge={avg_field(par,'merge_ns')/1e6:.3f}  "
        f"final={par_final/1e6:.3f}  validate={par_validate/1e6:.3f}  "
        f"write={par_write/1e6:.3f}{imputed_tag}"
    )
    print(f"  parallel_tax (grouping+merge par - grouping seq)={parallel_tax/1e6:.3f}ms")
    print(f"  shared (final+validate+write): seq={shared_seq/1e6:.3f}ms  par={shared_par/1e6:.3f}ms")
    print()
    print("=== serial / parallelizable (avg ms) ===")
    print(f"  seq  serial={serial_seq/1e6:.3f}  parallelizable={work_seq/1e6:.3f}  total={t_seq/1e6:.3f}")
    print(f"  par  serial={serial_par/1e6:.3f}  parallelizable={work_par/1e6:.3f}  total={t_par/1e6:.3f}")
    print()
    print("=== speedups ===")
    print(f"  achieved (T_seq/T_par)              = {achieved:.3f}x")
    print(f"  work efficiency (work_seq/ideal)    = {s_work:.3f}x  (cap from packing+uneven txs)")
    print(f"  theoretical (serial_par + ideal)    = {theoretical:.3f}x")
    print(f"  theoretical if parallel_tax→0       = {theoretical_no_tax:.3f}x")
    print()
    print("=== headroom ===")
    if math.isfinite(theoretical) and achieved > 0:
        print(f"  vs theoretical:           {(theoretical/achieved - 1)*100:.1f}% more possible (eff + same serial_par)")
    if math.isfinite(theoretical_no_tax) and achieved > 0:
        print(f"  vs no-tax ceiling:        {(theoretical_no_tax/achieved - 1)*100:.1f}% more possible")
    print()
    print(f"=== wave size histogram (avg count per parallel sample, n={n}) ===")
    for size in sorted(hist):
        print(f"  size {size:4d}: {hist[size] / n:.2f}")

    if eff_values:
        print()
        print("=== per-wave efficiency ===")
        print(
            f"  E = sum(seq tx in wave) / (concurrent_wall * min(W, |wave|)); "
            f"waves with concurrent_wall=0 are dropped"
        )
        print(f"  mean E={mean(eff_values):.3f}  over {len(eff_values)} wave-samples")
        by_size: dict[int, list[float]] = {}
        for s, e in zip(eff_sizes, eff_values):
            by_size.setdefault(s, []).append(e)
        for size in sorted(by_size):
            print(f"  size {size:4d}: mean E={mean(by_size[size]):.3f}  (n={len(by_size[size])})")
        plot_wave_efficiency(eff_sizes, eff_values, workers, plot_path.resolve())


def main() -> None:
    timing_path = TIMING_JSONL.resolve()
    # Re-plot / re-summarize existing JSONL without re-running the Go bench.
    if "--analyze-only" in sys.argv:
        if not timing_path.is_file():
            print(f"No timing file at {timing_path}", file=sys.stderr)
            sys.exit(1)
        print("\n=== Amdahl analysis (existing JSONL) ===\n")
        analyze(load_samples(timing_path))
        return

    timing_path.parent.mkdir(parents=True, exist_ok=True)
    if timing_path.exists():
        timing_path.unlink()

    env = os.environ.copy()
    env["BRANCH_NAME"] = BRANCH_NAME
    env["BENCHMARK_RUNS"] = str(RUNS)
    env["BENCHMARK_OUTPUT_FILE_REAL_BLOCKS"] = str(BENCH_LOG.resolve())
    env["PARALLEL_TX_TIMING"] = "1"
    env["PARALLEL_TX_TIMING_FILE"] = str(timing_path)

    input_dir = BENCHMARK_JSON_INPUT_DIR.resolve()
    temp_dir: tempfile.TemporaryDirectory[str] | None = None
    try:
        if JSON_FILE:
            json_path = input_dir / JSON_FILE
            if not json_path.is_file():
                print(f"JSON file not found: {json_path}", file=sys.stderr)
                sys.exit(1)
            temp_dir = tempfile.TemporaryDirectory(prefix="amdahl-bench-")
            shutil.copy2(json_path, Path(temp_dir.name) / JSON_FILE)
            env["BENCHMARK_JSON_INPUT_DIR"] = temp_dir.name
            print(f"Using single JSON: {json_path}")
        else:
            env["BENCHMARK_JSON_INPUT_DIR"] = str(input_dir)

        cmd = [
            "go",
            "test",
            "-v",
            "./benchmarks",
            "-count=1",
            "-run",
            "TestParallelBenchmarkAgainstRealBlocks",
            "-timeout",
            BENCHMARK_TIMEOUT,
        ]
        print(f"Timing JSONL -> {timing_path}")
        print(f"Running {RUNS} paired samples: {' '.join(cmd)}")
        result = subprocess.run(cmd, cwd=REPO_ROOT, env=env)
        if result.returncode != 0:
            sys.exit(result.returncode)
    finally:
        if temp_dir is not None:
            temp_dir.cleanup()

    if not timing_path.is_file():
        print(f"No timing file written at {timing_path}", file=sys.stderr)
        sys.exit(1)

    print("\n=== Amdahl analysis ===\n")
    analyze(load_samples(timing_path))


if __name__ == "__main__":
    main()
