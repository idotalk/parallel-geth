#!/usr/bin/env python3
"""Benchmark every real-block fixture with 1, 2, 4, and 8 workers."""

from __future__ import annotations

import json
import math
import os
import re
import shutil
import statistics
import subprocess
import sys
import tempfile
from pathlib import Path

try:
    import pandas as pd
except ImportError:
    sys.exit("pandas is required: python -m pip install pandas")


REPO_ROOT = Path(__file__).resolve().parent
BLOCKS_DIR = REPO_ROOT / "benchmarks" / "blocks" / "blocksdata"
RESULTS_DIR = REPO_ROOT / "benchmarks" / "results"
RAW_DIR = RESULTS_DIR / "worker_scaling_raw"
BY_BLOCK_CSV = RESULTS_DIR / "worker_scaling_by_block.csv"
SUMMARY_CSV = RESULTS_DIR / "worker_scaling_summary.csv"

WORKERS = (6,)
GOMAXPROCS = 12
RUNS = 100
BENCHMARK_TIMEOUT = "30m"
BRANCH_NAME = "worker-scaling"

METRICS = ("speedup", "achieved", "theoretical", "theoretical_no_tax")
SPEEDUP_RE = re.compile(r"Speedup: avg=([0-9.eE+-]+)x std=([0-9.eE+-]+)x")
WORKER_FLAG_RE = re.compile(r"var ParallelTxWorkers = \d+")


def mean_std(values: list[float]) -> tuple[float, float]:
    clean = [value for value in values if math.isfinite(value)]
    if not clean:
        return math.nan, math.nan
    return statistics.mean(clean), statistics.stdev(clean) if len(clean) > 1 else 0.0


def makespan_lower_bound(costs: list[int], workers: int) -> int:
    return max(max(costs), math.ceil(sum(costs) / workers)) if costs else 0


def load_jsonl(path: Path) -> list[dict]:
    with path.open(encoding="utf-8") as source:
        return [json.loads(line) for line in source if line.strip()]


def amdahl_sample_metrics(samples: list[dict], expected_workers: int) -> dict[str, tuple[float, float]]:
    seq = [sample for sample in samples if sample.get("mode") == "sequential"]
    par = [sample for sample in samples if sample.get("mode") == "parallel"]
    if len(seq) != len(par) or not seq:
        raise RuntimeError(f"expected paired timings, got sequential={len(seq)} parallel={len(par)}")
    if any(int(sample["workers"]) != expected_workers for sample in par):
        raise RuntimeError("timing JSONL recorded an unexpected worker count")

    tx_count = int(seq[0]["tx_count"])
    avg_tx_ns = [
        round(statistics.mean(float(sample["tx_apply_ns"][i]) for sample in seq))
        for i in range(tx_count)
    ]

    achieved: list[float] = []
    theoretical: list[float] = []
    theoretical_no_tax: list[float] = []
    for sequential, parallel in zip(seq, par):
        shared_seq = sum(float(sequential.get(key, 0)) for key in ("finalization_ns", "validate_ns", "write_ns"))
        # A failed parallel insertion has no meaningful post-Process timings.
        shared_par = (
            sum(float(parallel.get(key, 0)) for key in ("finalization_ns", "validate_ns", "write_ns"))
            if parallel.get("complete", True)
            else shared_seq
        )
        serial_seq = float(sequential.get("grouping_ns", 0)) + shared_seq
        serial_par = (
            float(parallel.get("grouping_ns", 0))
            + float(parallel.get("merge_ns", 0))
            + shared_par
        )
        seq_total = serial_seq + float(sequential["parallelizable_ns"])
        par_total = serial_par + float(parallel["parallelizable_ns"])

        ideal_work = sum(
            makespan_lower_bound([avg_tx_ns[i] for i in wave["txs"]], expected_workers)
            for wave in parallel["waves"]
        )
        achieved.append(seq_total / par_total if par_total else math.nan)
        theoretical.append(seq_total / (serial_par + ideal_work) if serial_par + ideal_work else math.nan)
        theoretical_no_tax.append(seq_total / (shared_par + ideal_work) if shared_par + ideal_work else math.nan)

    return {
        "achieved": mean_std(achieved),
        "theoretical": mean_std(theoretical),
        "theoretical_no_tax": mean_std(theoretical_no_tax),
    }


def parse_speedup(log_path: Path) -> tuple[float, float]:
    matches = SPEEDUP_RE.findall(log_path.read_text(encoding="utf-8"))
    if len(matches) != 1:
        raise RuntimeError(f"expected one speedup summary in {log_path}, found {len(matches)}")
    return tuple(map(float, matches[0]))  # type: ignore[return-value]


def save_tables(rows: list[dict]) -> tuple[pd.DataFrame, pd.DataFrame]:
    by_block = pd.DataFrame(rows).sort_values(["workers", "block"]).reset_index(drop=True)
    by_block.to_csv(BY_BLOCK_CSV, index=False)

    mean_columns = [f"{metric}_mean" for metric in METRICS]
    summary = by_block.groupby("workers", as_index=False)[mean_columns].mean()
    summary.columns = ["workers", *METRICS]
    summary.to_csv(SUMMARY_CSV, index=False)
    return by_block, summary


def print_tables(by_block: pd.DataFrame, summary: pd.DataFrame) -> None:
    printable = by_block[["workers", "block"]].copy()
    for metric in METRICS:
        printable[metric] = by_block.apply(
            lambda row: f"{row[f'{metric}_mean']:.3f} ± {row[f'{metric}_std']:.3f}", axis=1
        )
    print("\nPer block (sample mean ± std):")
    print(printable.to_string(index=False))
    print("\nAverage across blocks:")
    print(summary.to_string(index=False, float_format=lambda value: f"{value:.3f}"))


def run_one(block: Path, workers: int) -> dict:
    stem = f"{block.stem}_w{workers}"
    timing_path = RAW_DIR / f"{stem}.jsonl"
    log_path = RAW_DIR / f"{stem}.log"
    timing_path.unlink(missing_ok=True)
    log_path.unlink(missing_ok=True)

    with tempfile.TemporaryDirectory(prefix="worker-scaling-") as temp:
        shutil.copy2(block, Path(temp) / block.name)
        env = os.environ.copy()
        env.update(
            {
                "BENCHMARK_MODE": "both",
                "GOMAXPROCS": str(GOMAXPROCS),
                "BRANCH_NAME": BRANCH_NAME,
                "BENCHMARK_RUNS": str(RUNS),
                "BENCHMARK_JSON_INPUT_DIR": temp,
                "BENCHMARK_OUTPUT_FILE_REAL_BLOCKS": str(log_path),
                "PARALLEL_TX_TIMING": "1",
                "PARALLEL_TX_TIMING_FILE": str(timing_path),
            }
        )
        subprocess.run(
            [
                "go",
                "test",
                "-v",
                "./benchmarks",
                "-count=1",
                "-run",
                "TestParallelBenchmarkAgainstRealBlocks",
                "-timeout",
                BENCHMARK_TIMEOUT,
            ],
            cwd=REPO_ROOT,
            env=env,
            check=True,
        )

    speedup = parse_speedup(log_path)
    metrics = amdahl_sample_metrics(load_jsonl(timing_path), workers)
    metrics["speedup"] = speedup
    row: dict[str, float | int | str] = {"workers": workers, "block": block.name}
    for metric in METRICS:
        row[f"{metric}_mean"], row[f"{metric}_std"] = metrics[metric]
    return row


def main() -> None:
    blocks = sorted(BLOCKS_DIR.glob("*.json"))
    if not blocks:
        sys.exit(f"no JSON fixtures found in {BLOCKS_DIR}")

    RESULTS_DIR.mkdir(parents=True, exist_ok=True)
    RAW_DIR.mkdir(parents=True, exist_ok=True)
    flags_path = REPO_ROOT / "core" / "parallel_flags.go"
    original_flags = flags_path.read_text(encoding="utf-8")
    rows: list[dict] = []
    try:
        for workers in WORKERS:
            updated, replacements = WORKER_FLAG_RE.subn(
                f"var ParallelTxWorkers = {workers}", original_flags
            )
            if replacements != 1:
                raise RuntimeError("ParallelTxWorkers flag not found")
            flags_path.write_text(updated, encoding="utf-8")
            for index, block in enumerate(blocks, 1):
                print(f"\n[{workers} workers] block {index}/{len(blocks)}: {block.name}")
                rows.append(run_one(block, workers))
                save_tables(rows)  # checkpoint after every long benchmark
    finally:
        flags_path.write_text(original_flags, encoding="utf-8")

    by_block, summary = save_tables(rows)
    print_tables(by_block, summary)
    print(f"\nSaved:\n  {BY_BLOCK_CSV}\n  {SUMMARY_CSV}")


if __name__ == "__main__":
    main()
