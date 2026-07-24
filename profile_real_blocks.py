#!/usr/bin/env python3
"""CPU-profile sequential and parallel real-block runs into separate files."""

from __future__ import annotations

import os
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent
BENCHMARK_JSON_INPUT_DIR = REPO_ROOT / "benchmarks" / "blocks" / "blocksdata"
JSON_FILE = "25560620.json"
OUT_DIR = REPO_ROOT / "benchmarks" / "results"
BRANCH_NAME = "local"
RUNS = 100
TIMEOUT = "30m"
os.environ["GOMAXPROCS"] = "8"


def run_mode(mode: str, profile: Path, env: dict[str, str]) -> None:
    cmd = [
        "go",
        "test",
        "./benchmarks",
        "-count=1",
        "-run",
        "TestParallelBenchmarkAgainstRealBlocks",
        "-cpuprofile",
        str(profile),
        "-timeout",
        TIMEOUT,
    ]
    env = env.copy()
    env["BENCHMARK_MODE"] = mode
    print(f"\n=== profiling {mode} -> {profile.name} ===")
    subprocess.run(cmd, cwd=REPO_ROOT, env=env, check=True)
    summary = profile.with_suffix(".txt")
    with summary.open("w", encoding="utf-8") as out:
        subprocess.run(
            ["go", "tool", "pprof", "-top", "-cum", str(profile)],
            cwd=REPO_ROOT,
            stdout=out,
            check=True,
        )
    print(f"summary -> {summary}")


def main() -> None:
    OUT_DIR.mkdir(parents=True, exist_ok=True)
    env = os.environ.copy()
    env["BRANCH_NAME"] = BRANCH_NAME
    env["BENCHMARK_RUNS"] = str(RUNS)
    env["BENCHMARK_OUTPUT_FILE_REAL_BLOCKS"] = str(OUT_DIR / "_temp_profile_runs.log")

    input_dir = BENCHMARK_JSON_INPUT_DIR.resolve()
    temp_dir: tempfile.TemporaryDirectory[str] | None = None
    try:
        if JSON_FILE:
            temp_dir = tempfile.TemporaryDirectory(prefix="real-blocks-profile-")
            shutil.copy2(input_dir / JSON_FILE, Path(temp_dir.name) / JSON_FILE)
            env["BENCHMARK_JSON_INPUT_DIR"] = temp_dir.name
        else:
            env["BENCHMARK_JSON_INPUT_DIR"] = str(input_dir)

        run_mode("sequential", OUT_DIR / "cpu_sequential.out", env)
        run_mode("parallel", OUT_DIR / "cpu_parallel.out", env)
    finally:
        if temp_dir is not None:
            temp_dir.cleanup()


if __name__ == "__main__":
    main()
