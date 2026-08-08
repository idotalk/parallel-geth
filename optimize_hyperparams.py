#!/usr/bin/env python3
"""Hyperparameter optimization for parallel-geth.

Iterates over combinations of ParallelTxWorkers and
ParallelTxDirectExecutionMaxWaveSize, runs the Amdahl analysis for each,
and reports the best configuration based on maximizing the theoretical speedup cap.
"""
from __future__ import annotations

import json
import os
import re
import shutil
import subprocess
import sys
import tempfile
import time
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent
FLAGS_FILE = REPO_ROOT / "core" / "parallel_flags.go"
BENCHMARK_JSON_INPUT_DIR = REPO_ROOT / "benchmarks" / "blocks" / "blocksdata"
JSON_FILE = "25603091.json"
TIMING_JSONL = REPO_ROOT / "benchmarks" / "results" / "amdahl_timing.jsonl"
BENCH_LOG = REPO_ROOT / "benchmarks" / "results" / "_temp_amdahl_bench.log"
BENCHMARK_TIMEOUT = "30m"
RUNS = 30  # fewer runs per config for faster iteration; still statistically meaningful

# Previous runs showed that the best values lay around the following options
WORKERS_GRID = [4, 5, 6, 7, 8]
DIRECT_EXEC_GRID = [4, 5, 6, 7, 8, 9]


def update_flags(workers: int, direct_exec_max: int) -> None:
    """Rewrite parallel_flags.go with new hyperparameter values."""
    content = FLAGS_FILE.read_text(encoding="utf-8")

    content = re.sub(
        r"var ParallelTxWorkers = \d+",
        f"var ParallelTxWorkers = {workers}",
        content,
    )
    content = re.sub(
        r"var ParallelTxDirectExecutionMaxWaveSize = \d+",
        f"var ParallelTxDirectExecutionMaxWaveSize = {direct_exec_max}",
        content,
    )
    FLAGS_FILE.write_text(content, encoding="utf-8")


def run_benchmark(workers: int, direct_exec_max: int) -> Path | None:
    """Run the Amdahl benchmark and return the timing JSONL path."""
    update_flags(workers, direct_exec_max)

    timing_path = TIMING_JSONL.resolve()
    timing_path.parent.mkdir(parents=True, exist_ok=True)
    if timing_path.exists():
        timing_path.unlink()

    env = os.environ.copy()
    env["BENCHMARK_MODE"] = "both"
    env["GOMAXPROCS"] = str(max(workers, 8))  # ensure GOMAXPROCS >= workers
    env["PARALLEL_TX_TIMING"] = "1"
    env["BRANCH_NAME"] = "local"
    env["BENCHMARK_RUNS"] = str(RUNS)
    env["BENCHMARK_OUTPUT_FILE_REAL_BLOCKS"] = str(BENCH_LOG.resolve())
    env["PARALLEL_TX_TIMING_FILE"] = str(timing_path)
    env["PYTHONIOENCODING"] = "utf-8"

    input_dir = BENCHMARK_JSON_INPUT_DIR.resolve()
    temp_dir = None
    try:
        if JSON_FILE:
            json_path = input_dir / JSON_FILE
            if not json_path.is_file():
                print(f"JSON file not found: {json_path}", file=sys.stderr)
                return None
            temp_dir = tempfile.TemporaryDirectory(prefix="opt-bench-")
            shutil.copy2(json_path, Path(temp_dir.name) / JSON_FILE)
            env["BENCHMARK_JSON_INPUT_DIR"] = temp_dir.name

        cmd = [
            "go", "test", "-v", "./benchmarks", "-count=1",
            "-run", "TestParallelBenchmarkAgainstRealBlocks",
            "-timeout", BENCHMARK_TIMEOUT,
        ]
        print(f"\n{'='*70}")
        print(f"CONFIG: Workers={workers}, DirectExecMax={direct_exec_max}, Runs={RUNS}")
        print(f"{'='*70}")
        result = subprocess.run(cmd, cwd=REPO_ROOT, env=env, capture_output=True, text=True)
        if result.returncode != 0:
            print(f"Benchmark FAILED (rc={result.returncode})", file=sys.stderr)
            if result.stderr:
                # Print last few lines of stderr for diagnostics
                for line in result.stderr.strip().split('\n')[-5:]:
                    print(f"  stderr: {line}", file=sys.stderr)
            return None
    finally:
        if temp_dir is not None:
            temp_dir.cleanup()

    if not timing_path.is_file():
        print(f"No timing file at {timing_path}", file=sys.stderr)
        return None

    return timing_path


def parse_results(timing_path: Path) -> dict | None:
    """Run analyze and parse the key metrics from stdout."""
    env = os.environ.copy()
    env["PYTHONIOENCODING"] = "utf-8"
    result = subprocess.run(
        [sys.executable, str(REPO_ROOT / "run_amdahl_analysis.py"), "--analyze-only"],
        cwd=REPO_ROOT,
        env=env,
        capture_output=True,
        text=True,
        encoding="utf-8",
    )

    output = result.stdout
    if result.returncode != 0:
        print(f"Analysis failed: {result.stderr[:500]}", file=sys.stderr)
        return None

    print(output)  # show the full analysis

    metrics: dict = {}

    # Parse achieved speedup
    m = re.search(r"achieved \(T_seq/T_par\)\s*=\s*([\d.]+)x", output)
    if m:
        metrics["achieved"] = float(m.group(1))

    # Parse theoretical
    m = re.search(r"theoretical \(serial_par \+ ideal\)\s*=\s*([\d.]+)x", output)
    if m:
        metrics["theoretical"] = float(m.group(1))

    # Parse theoretical no tax
    m = re.search(r"theoretical if parallel_tax.*=\s*([\d.]+)x", output)
    if m:
        metrics["theoretical_no_tax"] = float(m.group(1))

    # Parse work efficiency
    m = re.search(r"work efficiency.*=\s*([\d.]+)x", output)
    if m:
        metrics["work_efficiency"] = float(m.group(1))

    # Parse parallel tax
    m = re.search(r"parallel_tax.*=([\d.]+)ms", output)
    if m:
        metrics["parallel_tax_ms"] = float(m.group(1))

    # Parse mean wave efficiency
    m = re.search(r"mean E=([\d.]+)", output)
    if m:
        metrics["mean_wave_efficiency"] = float(m.group(1))

    # Parse total times
    m = re.search(r"seq\s+serial=([\d.]+)\s+parallelizable=([\d.]+)\s+total=([\d.]+)", output)
    if m:
        metrics["seq_total_ms"] = float(m.group(3))
    m = re.search(r"par\s+serial=([\d.]+)\s+parallelizable=([\d.]+)\s+total=([\d.]+)", output)
    if m:
        metrics["par_total_ms"] = float(m.group(3))
        metrics["par_serial_ms"] = float(m.group(1))
        metrics["par_parallelizable_ms"] = float(m.group(2))

    return metrics if ("theoretical" in metrics and "achieved" in metrics) else None


def main() -> None:
    results: list[dict] = []
    results_file = REPO_ROOT / "benchmarks" / "results" / "hyperopt_results.json"

    print("=" * 70)
    print("PARALLEL-GETH HYPERPARAMETER OPTIMIZATION (THEORETICAL CAP MAXIMIZATION)")
    print(f"Block: {JSON_FILE}")
    print(f"Workers grid: {WORKERS_GRID}")
    print(f"DirectExecMax grid: {DIRECT_EXEC_GRID}")
    print(f"Runs per config: {RUNS}")
    print(f"Total configs: {len(WORKERS_GRID) * len(DIRECT_EXEC_GRID)}")
    print("=" * 70)

    for workers in WORKERS_GRID:
        for direct_exec in DIRECT_EXEC_GRID:
            start = time.time()
            timing_path = run_benchmark(workers, direct_exec)
            elapsed = time.time() - start

            if timing_path is None:
                print(f"SKIP Workers={workers} DirectExec={direct_exec} (benchmark failed)")
                continue

            metrics = parse_results(timing_path)
            if metrics is None:
                print(f"SKIP Workers={workers} DirectExec={direct_exec} (parse failed)")
                continue

            entry = {
                "workers": workers,
                "direct_exec_max": direct_exec,
                "elapsed_s": round(elapsed, 1),
                **metrics,
            }
            results.append(entry)

            # Save incrementally
            results_file.write_text(
                json.dumps(results, indent=2), encoding="utf-8"
            )

            print(f"\n>>> Workers={workers} DirectExec={direct_exec}: "
                  f"theoretical={metrics.get('theoretical', 0):.3f}x  "
                  f"achieved={metrics['achieved']:.3f}x  "
                  f"tax={metrics.get('parallel_tax_ms', 0):.1f}ms  "
                  f"({elapsed:.0f}s)\n")

    # Summary
    print("\n" + "=" * 70)
    print("OPTIMIZATION COMPLETE - RANKED BY THEORETICAL CAP")
    print("=" * 70)

    if not results:
        print("No successful runs!")
        return

    # Sort primarily by theoretical cap, secondarily by achieved speedup
    results.sort(key=lambda r: (r.get("theoretical", 0), r.get("achieved", 0)), reverse=True)

    print(f"\n{'Workers':>8} {'DirExec':>8} {'Theoret':>10} {'Achieved':>10} "
          f"{'Tax(ms)':>10} {'WaveEff':>10} {'ParTime':>10}")
    print("-" * 70)
    for r in results:
        print(f"{r['workers']:>8} {r['direct_exec_max']:>8} "
              f"{r.get('theoretical', 0):>10.3f}x "
              f"{r.get('achieved', 0):>10.3f}x "
              f"{r.get('parallel_tax_ms', 0):>10.1f} "
              f"{r.get('mean_wave_efficiency', 0):>10.3f} "
              f"{r.get('par_total_ms', 0):>10.1f}")

    best = results[0]
    print(f"\nBEST CONFIG: Workers={best['workers']}, "
          f"DirectExecMax={best['direct_exec_max']} -> "
          f"{best['theoretical']:.3f}x theoretical cap ({best['achieved']:.3f}x achieved)")

    # Apply best config
    update_flags(best["workers"], best["direct_exec_max"])
    print(f"\nApplied best config to {FLAGS_FILE}")
    print(f"Full results saved to {results_file}")


if __name__ == "__main__":
    main()

