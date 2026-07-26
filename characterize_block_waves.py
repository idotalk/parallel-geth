#!/usr/bin/env python3
"""Characterize Ethereum blocks using Go wave construction algorithm & Sequential Tail Analysis.

This script parses block JSON files, constructs transaction waves following
BuildTransactionStorageParallelGroups in core/parallel_processor.go, and evaluates:
1. Structural parallelizability (waves >= 2)
2. Threshold-aware parallelizability (waves > M, e.g. M=9)
3. Suffix Sequential Tail (end-of-block serialization)
4. Pairwise conflict graph density
5. Integration with Amdahl speedup caps (when timing JSONL is available)
"""

from __future__ import annotations

import json
import math
import os
import sys
from collections import Counter
from pathlib import Path
from typing import Any, Dict, List, Set, Tuple

REPO_ROOT = Path(__file__).resolve().parent
BLOCKS_DIR = REPO_ROOT / "benchmarks" / "blocks" / "blocksdata"
TIMING_JSONL = REPO_ROOT / "benchmarks" / "results" / "amdahl_timing.jsonl"
OUTPUT_REPORT = REPO_ROOT / "benchmarks" / "results" / "block_characterization.md"
WAVE_PROFILE_PLOT = REPO_ROOT / "benchmarks" / "results" / "wave_profiles.png"

# Default direct execution threshold (ParallelTxDirectExecutionMaxWaveSize)
DEFAULT_MAX_DIRECT_WAVE_SIZE = 9


def normalize_address(addr: str | None) -> str | None:
    if not addr:
        return None
    addr = addr.lower().strip()
    if addr.startswith("0x"):
        addr = addr[2:]
    return addr.zfill(40)


def extract_tx_declared_addresses(tx: Dict[str, Any]) -> Set[str]:
    """Extract set of declared addresses for a transaction.
    
    Includes:
    - from (if present or derived)
    - to (if present)
    - accessList / declaredAccessList / generatedAccessList
    """
    addrs: Set[str] = set()

    # Recipient
    to_addr = normalize_address(tx.get("to"))
    if to_addr:
        addrs.add(to_addr)

    # Sender (if present in json)
    from_addr = normalize_address(tx.get("from"))
    if from_addr:
        addrs.add(from_addr)

    # Access Lists
    for acl_key in ("accessList", "declaredAccessList", "generatedAccessList"):
        acl = tx.get(acl_key)
        if isinstance(acl, list):
            for entry in acl:
                if isinstance(entry, dict):
                    a = normalize_address(entry.get("address"))
                    if a:
                        addrs.add(a)

    return addrs


def build_parallel_waves(tx_addrs: List[Set[str]]) -> List[List[int]]:
    """Replicate BuildTransactionStorageParallelGroups from core/parallel_processor.go.
    
    Greedy wave partition in canonical tx order with predecessor conflict rule.
    """
    n = len(tx_addrs)
    if n == 0:
        return []

    unassigned = [True] * n
    waves: List[List[int]] = []

    while True:
        seed = -1
        for i in range(n):
            if unassigned[i]:
                seed = i
                break
        if seed == -1:
            break

        wave = [seed]
        wave_addrs = set(tx_addrs[seed])
        unassigned[seed] = False

        for j in range(seed + 1, n):
            if not unassigned[j]:
                continue
            cand_addrs = tx_addrs[j]

            # 1. Check overlap with current wave
            if not wave_addrs.isdisjoint(cand_addrs):
                continue

            # 2. Causality / Predecessor constraint: any earlier pending tx k < j
            # that conflicts with j must block j from entering this wave.
            blocked = False
            for k in range(j):
                if unassigned[k] and not tx_addrs[k].isdisjoint(cand_addrs):
                    blocked = True
                    break

            if blocked:
                continue

            # Add to wave
            wave.append(j)
            unassigned[j] = False
            wave_addrs.update(cand_addrs)

        waves.append(wave)

    return waves


def calculate_conflict_graph(tx_addrs: List[Set[str]]) -> Tuple[int, float]:
    """Calculate total conflicting pairs and conflict graph density."""
    n = len(tx_addrs)
    if n <= 1:
        return 0, 0.0

    conflicting_pairs = 0
    for i in range(n):
        for j in range(i + 1, n):
            if not tx_addrs[i].isdisjoint(tx_addrs[j]):
                conflicting_pairs += 1

    total_pairs = n * (n - 1) // 2
    density = conflicting_pairs / total_pairs if total_pairs > 0 else 0.0
    return conflicting_pairs, density


def analyze_sequential_tail(waves: List[List[int]], threshold: int) -> Dict[str, Any]:
    """Analyze the suffix sequential tail for a given wave execution threshold M.
    
    A wave is parallelized only if len(wave) > threshold.
    Suffix sequential tail begins after the last wave with len(wave) > threshold.
    """
    last_parallel_idx = -1
    for idx, wave in enumerate(waves):
        if len(wave) > threshold:
            last_parallel_idx = idx

    if last_parallel_idx == -1:
        # Entire block is sequential
        tail_waves = waves
        tail_tx_count = sum(len(w) for w in waves)
    else:
        tail_waves = waves[last_parallel_idx + 1:]
        tail_tx_count = sum(len(w) for w in tail_waves)

    total_txs = sum(len(w) for w in waves)
    tail_ratio = tail_tx_count / total_txs if total_txs > 0 else 0.0

    return {
        "last_parallel_wave_index": last_parallel_idx,
        "tail_wave_count": len(tail_waves),
        "tail_tx_count": tail_tx_count,
        "tail_tx_ratio": tail_ratio,
        "tail_waves": tail_waves,
    }


def characterize_block_json(json_path: Path, threshold: int = DEFAULT_MAX_DIRECT_WAVE_SIZE) -> Dict[str, Any]:
    """Extract and analyze a single block JSON file."""
    with json_path.open("r", encoding="utf-8-sig") as f:
        data = json.load(f)

    # Unwrap test instance key
    instance = list(data.values())[0] if isinstance(data, dict) else data
    blocks = instance.get("blocks", [])
    if not blocks:
        raise ValueError(f"No blocks found in {json_path}")

    target_block = blocks[0]
    block_number = target_block.get("blockHeader", {}).get("number", "0x0")
    if isinstance(block_number, str) and block_number.startswith("0x"):
        block_num_int = int(block_number, 16)
    else:
        block_num_int = int(block_number)

    raw_txs = target_block.get("transactions", [])
    tx_addrs = [extract_tx_declared_addresses(tx) for tx in raw_txs]
    n = len(tx_addrs)

    waves = build_parallel_waves(tx_addrs)
    wave_sizes = [len(w) for w in waves]
    num_waves = len(waves)

    # Structural Parallelism (len >= 2)
    structural_par_txs = sum(len(w) for w in waves if len(w) >= 2)
    structural_par_ratio = structural_par_txs / n if n > 0 else 0.0

    # Threshold-Aware Parallelism (len > M)
    threshold_par_txs = sum(len(w) for w in waves if len(w) > threshold)
    threshold_par_ratio = threshold_par_txs / n if n > 0 else 0.0

    # Conflict Graph Metrics
    conflicting_pairs, conflict_density = calculate_conflict_graph(tx_addrs)

    # Sequential Tail
    tail_info = analyze_sequential_tail(waves, threshold)

    return {
        "file_name": json_path.name,
        "block_number": block_num_int,
        "tx_count": n,
        "wave_count": num_waves,
        "max_wave_size": max(wave_sizes) if wave_sizes else 0,
        "avg_wave_size": n / num_waves if num_waves > 0 else 0.0,
        "structural_par_txs": structural_par_txs,
        "structural_par_ratio": structural_par_ratio,
        "threshold_par_txs": threshold_par_txs,
        "threshold_par_ratio": threshold_par_ratio,
        "conflicting_pairs": conflicting_pairs,
        "conflict_density": conflict_density,
        "wave_sizes": wave_sizes,
        "tail_info": tail_info,
    }


def plot_wave_profiles(block_stats: List[Dict[str, Any]], threshold: int, out_path: Path) -> None:
    """Generate wave size profiles across wave index for blocks."""
    try:
        import matplotlib.pyplot as plt
    except ImportError:
        print("matplotlib not installed; skipping wave profile plot", file=sys.stderr)
        return

    num_blocks = len(block_stats)
    cols = min(2, num_blocks)
    rows = math.ceil(num_blocks / cols)

    fig, axes = plt.subplots(rows, cols, figsize=(7 * cols, 3.5 * rows), squeeze=False)
    fig.suptitle(f"Block Wave Size Profiles & Sequential Tail (Threshold M={threshold})", fontsize=14, fontweight="bold")

    for idx, stat in enumerate(block_stats):
        r, c = divmod(idx, cols)
        ax = axes[r][c]

        wave_sizes = stat["wave_sizes"]
        tail_start = stat["tail_info"]["last_parallel_wave_index"] + 1
        wave_indices = list(range(1, len(wave_sizes) + 1))

        # Separate parallel vs tail wave sizes
        if tail_start < len(wave_sizes):
            par_x = wave_indices[:tail_start]
            par_y = wave_sizes[:tail_start]
            tail_x = wave_indices[tail_start:]
            tail_y = wave_sizes[tail_start:]
        else:
            par_x = wave_indices
            par_y = wave_sizes
            tail_x = []
            tail_y = []

        if par_x:
            ax.bar(par_x, par_y, color="#2b5c8f", alpha=0.85, label="Parallel Waves")
        if tail_x:
            ax.bar(tail_x, tail_y, color="#d9534f", alpha=0.85, label="Sequential Tail Waves")

        ax.axhline(threshold, color="gray", linestyle="--", linewidth=1, label=f"Threshold M={threshold}")
        ax.set_xlabel("Wave Index k")
        ax.set_ylabel("Wave Size |W_k|")
        ax.set_title(f"Block #{stat['block_number']} - N={stat['tx_count']}, K={stat['wave_count']}")
        ax.grid(True, alpha=0.2)
        ax.legend(loc="upper right", fontsize=8)

    # Hide extra empty subplots
    for idx in range(num_blocks, rows * cols):
        r, c = divmod(idx, cols)
        axes[r][c].axis("off")

    fig.tight_layout()
    out_path.parent.mkdir(parents=True, exist_ok=True)
    fig.savefig(out_path, dpi=150)
    plt.close(fig)
    print(f"Wave profile plot saved -> {out_path}")


def main() -> None:
    threshold = DEFAULT_MAX_DIRECT_WAVE_SIZE
    if len(sys.argv) > 1:
        try:
            threshold = int(sys.argv[1])
        except ValueError:
            pass

    json_files = sorted(list(BLOCKS_DIR.glob("*.json")))
    if not json_files:
        print(f"No JSON block files found in {BLOCKS_DIR}", file=sys.stderr)
        sys.exit(1)

    print(f"Characterizing {len(json_files)} blocks with Direct Execution Threshold M={threshold}...\n")

    stats_list: List[Dict[str, Any]] = []
    for jf in json_files:
        try:
            res = characterize_block_json(jf, threshold)
            stats_list.append(res)
        except Exception as e:
            print(f"Error processing {jf.name}: {e}", file=sys.stderr)

    if not stats_list:
        print("No block data successfully processed.", file=sys.stderr)
        sys.exit(1)

    # Console Summary Table
    print("=" * 105)
    print(f"{'Block':>10} {'Txs(N)':>8} {'Waves(K)':>8} {'MaxW':>6} {'AvgW':>6} "
          f"{'Par(>=2)':>9} {'Par(>M)':>9} {'TailTxs':>8} {'Tail%':>8} {'Density':>8}")
    print("-" * 105)

    for s in stats_list:
        tail = s["tail_info"]
        print(f"{s['block_number']:>10} {s['tx_count']:>8} {s['wave_count']:>8} "
              f"{s['max_wave_size']:>6} {s['avg_wave_size']:>6.2f} "
              f"{s['structural_par_ratio']*100:>8.1f}% "
              f"{s['threshold_par_ratio']*100:>8.1f}% "
              f"{tail['tail_tx_count']:>8} {tail['tail_tx_ratio']*100:>7.1f}% "
              f"{s['conflict_density']:>8.4f}")
    print("=" * 105)

    plot_wave_profiles(stats_list, threshold, WAVE_PROFILE_PLOT)

    # Save Markdown Report
    OUTPUT_REPORT.parent.mkdir(parents=True, exist_ok=True)
    with OUTPUT_REPORT.open("w", encoding="utf-8") as f:
        f.write("# Block Parallelizability & Sequential Tail Characterization Report\n\n")
        f.write(f"**Execution Parameters**: Direct Execution Max Wave Size $M = {threshold}$\n\n")
        f.write("## Block Summary Table\n\n")
        f.write("| Block Number | File | Total Txs ($N$) | Wave Count ($K$) | Max Wave | Avg Wave | Structural Par ($|W_k| \\ge 2$) | Threshold Par ($|W_k| > M$) | Tail Txs ($N_{\\text{tail}}$) | Tail % | Conflict Density |\n")
        f.write("|---|---|---|---|---|---|---|---|---|---|---|\n")
        for s in stats_list:
            tail = s["tail_info"]
            f.write(f"| {s['block_number']} | `{s['file_name']}` | {s['tx_count']} | {s['wave_count']} | {s['max_wave_size']} | {s['avg_wave_size']:.2f} | {s['structural_par_ratio']*100:.1f}% | {s['threshold_par_ratio']*100:.1f}% | {tail['tail_tx_count']} | {tail['tail_tx_ratio']*100:.1f}% | {s['conflict_density']:.4f} |\n")

        f.write("\n## Key Definitions & Insights\n\n")
        f.write("- **Structural Parallelism ($|W_k| \\ge 2$)**: Transactions in waves containing multiple transactions.\n")
        f.write(f"- **Threshold-Aware Parallelism ($|W_k| > {threshold}$)**: Transactions executed concurrently under direct execution threshold $M={threshold}$.\n")
        f.write(f"- **Sequential Tail ($N_{{\\text{{tail}}}}$)**: Suffix waves following the final parallel wave ($|W_k| > {threshold}$) in the block.\n")
        f.write("- **Conflict Density**: Pairwise transaction account overlap ratio $\\frac{2E}{N(N-1)}$.\n")

    print(f"\nReport saved -> {OUTPUT_REPORT}")


if __name__ == "__main__":
    main()
