#!/usr/bin/env python3
"""Run TestParallelBenchmarkAgainstRealBlocks with in-script config."""

from __future__ import annotations

import os
import re
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

# ---------------------------------------------------------------------------
# CONFIG
# ---------------------------------------------------------------------------


os.environ["GOMAXPROCS"] = "8"
REPO_ROOT = Path(__file__).resolve().parent

# Directory containing block JSON files (used when JSON_FILE is empty).
BENCHMARK_JSON_INPUT_DIR = REPO_ROOT / "benchmarks" / "blocks" / "blocksdata"

# Set to a filename like "25560699.json" to run only that file; leave "" for all .json files.
JSON_FILE = "25560620.json"

BENCHMARK_OUTPUT_FILE_REAL_BLOCKS = REPO_ROOT / "benchmarks" / "results" / "_temp_real_blocks.log"

BRANCH_NAME = "local"
BENCHMARK_TIMEOUT = "30m"
RUNS = 100

# Empty uses the current Go flag. Use [3] for one value or list(range(9)) to sweep.
DIRECT_EXECUTION_MAX_WAVE_SIZES: list[int] = [4]

# ---------------------------------------------------------------------------

def main() -> None:
    output_path = BENCHMARK_OUTPUT_FILE_REAL_BLOCKS.resolve()
    output_path.parent.mkdir(parents=True, exist_ok=True)

    env = os.environ.copy()
    env["BRANCH_NAME"] = BRANCH_NAME
    env["BENCHMARK_RUNS"] = str(RUNS)
    env["BENCHMARK_OUTPUT_FILE_REAL_BLOCKS"] = str(output_path)
    input_dir = BENCHMARK_JSON_INPUT_DIR.resolve()
    if not input_dir.is_dir():
        print(f"Input directory not found: {input_dir}", file=sys.stderr)
        sys.exit(1)

    temp_dir: tempfile.TemporaryDirectory[str] | None = None
    if JSON_FILE:
        json_path = input_dir / JSON_FILE
        if not json_path.is_file():
            print(f"JSON file not found: {json_path}", file=sys.stderr)
            sys.exit(1)
        temp_dir = tempfile.TemporaryDirectory(prefix="real-blocks-bench-")
        shutil.copy2(json_path, Path(temp_dir.name) / JSON_FILE)
        env["BENCHMARK_JSON_INPUT_DIR"] = temp_dir.name
        print(f"Using single JSON: {json_path}")
    else:
        env["BENCHMARK_JSON_INPUT_DIR"] = str(input_dir)
        print(f"Using all JSON files in: {input_dir}")

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
    print(f"Summary output -> {output_path}")
    print(f"Running {RUNS} paired samples in Go: {' '.join(cmd)}")

    flags_path = REPO_ROOT / "core" / "parallel_flags.go"
    original_flags = flags_path.read_text()
    try:
        result = None
        for value in DIRECT_EXECUTION_MAX_WAVE_SIZES or [None]:
            if value is not None:
                updated_flags, replacements = re.subn(
                    r"var ParallelTxDirectExecutionMaxWaveSize = \d+",
                    f"var ParallelTxDirectExecutionMaxWaveSize = {value}",
                    original_flags,
                )
                if replacements != 1:
                    raise RuntimeError("ParallelTxDirectExecutionMaxWaveSize flag not found")
                flags_path.write_text(updated_flags)
                with output_path.open("a") as output:
                    output.write(f"\n# ParallelTxDirectExecutionMaxWaveSize={value}\n")
            result = subprocess.run(cmd, cwd=REPO_ROOT, env=env)
            if result.returncode:
                break
    finally:
        if DIRECT_EXECUTION_MAX_WAVE_SIZES:
            flags_path.write_text(original_flags)
        if temp_dir is not None:
            temp_dir.cleanup()

    assert result is not None
    sys.exit(result.returncode)


if __name__ == "__main__":
    main()
