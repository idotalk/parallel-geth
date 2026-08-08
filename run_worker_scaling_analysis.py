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

import pandas as pd


REPO_ROOT = Path(__file__).resolve().parent
BLOCKS_DIR = REPO_ROOT / "benchmarks" / "blocks" / "blocksdata"
RESULTS_DIR = REPO_ROOT / "benchmarks" / "results"
RAW_DIR = RESULTS_DIR / "worker_scaling_raw"
BY_BLOCK_CSV = RESULTS_DIR / "worker_scaling_by_block.csv"
SUMMARY_CSV = RESULTS_DIR / "worker_scaling_summary.csv"

WORKERS = (2, 4, 6, 8)
GOMAXPROCS = 12
RUNS = 100
BENCHMARK_TIMEOUT = "30m"
BRANCH_NAME = "worker-scaling"

METRICS = (
    "speedup",
    "achieved",
    "theoretical",
    "theoretical_no_tax",
    "theoretical_no_dep",
    "theoretical_no_serial",
)
SPEEDUP_RE = re.compile(r"Speedup: avg=([0-9.eE+-]+)x std=([0-9.eE+-]+)x")
WORKER_FLAG_RE = re.compile(r"var ParallelTxWorkers = \d+")


def finite_mean(values: list[float]) -> float:
    clean = [value for value in values if math.isfinite(value)]
    return statistics.mean(clean) if clean else math.nan


def makespan_lower_bound(costs: list[int], workers: int) -> int:
    return max(max(costs), math.ceil(sum(costs) / workers)) if costs else 0


def load_jsonl(path: Path) -> list[dict]:
    with path.open(encoding="utf-8") as source:
        return [json.loads(line) for line in source if line.strip()]


def amdahl_sample_metrics(samples: list[dict], expected_workers: int) -> dict[str, float]:
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
    theoretical_no_dep: list[float] = []
    theoretical_no_serial: list[float] = []
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
        ideal_single_wave = makespan_lower_bound(avg_tx_ns, expected_workers)
        parallel_tax = float(parallel.get("grouping_ns", 0)) + float(parallel.get("merge_ns", 0))
        achieved.append(seq_total / par_total if par_total else math.nan)
        theoretical.append(seq_total / (serial_par + ideal_work) if serial_par + ideal_work else math.nan)
        theoretical_no_tax.append(seq_total / (shared_par + ideal_work) if shared_par + ideal_work else math.nan)
        theoretical_no_dep.append(
            seq_total / (serial_par + ideal_single_wave) if serial_par + ideal_single_wave else math.nan
        )
        theoretical_no_serial.append(
            seq_total / (parallel_tax + ideal_work) if parallel_tax + ideal_work else math.nan
        )

    return {
        "achieved": finite_mean(achieved),
        "theoretical": finite_mean(theoretical),
        "theoretical_no_tax": finite_mean(theoretical_no_tax),
        "theoretical_no_dep": finite_mean(theoretical_no_dep),
        "theoretical_no_serial": finite_mean(theoretical_no_serial),
    }


def parse_speedup(log_path: Path) -> float:
    matches = SPEEDUP_RE.findall(log_path.read_text(encoding="utf-8"))
    if len(matches) != 1:
        raise RuntimeError(f"expected one speedup summary in {log_path}, found {len(matches)}")
    return float(matches[0][0])


def save_tables(rows: list[dict]) -> tuple[pd.DataFrame, pd.DataFrame]:
    by_block = pd.DataFrame(rows).sort_values(["workers", "block"]).reset_index(drop=True)
    by_block.to_csv(BY_BLOCK_CSV, index=False)

    summary = by_block.groupby("workers", as_index=False)[list(METRICS)].mean()
    summary.to_csv(SUMMARY_CSV, index=False)
    return by_block, summary


def print_tables(by_block: pd.DataFrame, summary: pd.DataFrame) -> None:
    print("\nPer block:")
    print(by_block.to_string(index=False, float_format=lambda value: f"{value:.3f}"))
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
        row[metric] = metrics[metric]
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
