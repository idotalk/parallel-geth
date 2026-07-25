# Parallel Geth — How to Run

Flags: `core/parallel_flags.go`

| Flag | Role |
|------|------|
| `ParallelTxGroupingByStorageOverlap` | Wave packing by declared address disjointness |
| `ParallelTxWaveExecution` | Concurrent execution within a wave |
| `ParallelTxWorkers` | Max goroutines / StateDB forks per wave (`≤0` → `GOMAXPROCS`) |
| `ParallelTxDirectExecutionMaxWaveSize` | Waves ≤ N run on shared state (no clone) |
| `ParallelTxDebug` | Access-list / grouping debug prints |
| `ParallelTxTiming` | Buffer Process (and validate/write) timings for JSONL |

Bench paths: sequential turns grouping + wave execution **off**; parallel turns both **on**. Parallel waves use up to `ParallelTxWorkers` workers (each clones once, reuses the dirty fork). Waves ≤ `ParallelTxDirectExecutionMaxWaveSize` stay on shared state. Prefetch is skipped when both parallel flags are on.

---

## Correctness

```powershell
go test ./tests -count=1 -timeout 600m -v -run TestBlockchain
go test ./tests -count=1 -v -run "TestParallelVM"
go test ./core -count=1 -v -run TestBuildTransactionStorageParallelGroupsCausalOrder
```

Also: `tests/parallel_vm_test.go`, `tests/parallel_vm_block_test.go`.

---

## Synthetic benchmarks

`tests/parallel_execution_bench_test.go` → `TestParallelBenchmarkOutput`  
Tx counts `{50,150,300,450,600}` × `{Isolated,Contended,Mixed}`.

```powershell
$env:BENCHMARK_OUTPUT_FILE="$PWD\optimization_logs\_temp_run.log"
$env:BRANCH_NAME="local"
go test -v ./tests -count=1 -run TestParallelBenchmarkOutput -timeout 60m
```

Skips if `BENCHMARK_OUTPUT_FILE` unset.

---

## Real-block benchmarks

```powershell
python .\run_real_blocks_benchmark.py
```

Edit constants at top of the script (`JSON_FILE`, `RUNS`, `DIRECT_EXECUTION_MAX_WAVE_SIZES`, …).  
Test: `benchmarks/paralell_bench_realBlocks_test.go`. Fixtures: `benchmarks/blocks/blocksdata/` (prefer JSONs with `pre`).

- Warm once per file, then `BENCHMARK_RUNS` timed samples; mode `both` alternates seq/par.
- Appends summaries to `BENCHMARK_OUTPUT_FILE_REAL_BLOCKS`.

---

## Amdahl / timing analysis

```powershell
python .\run_amdahl_analysis.py
python .\run_amdahl_analysis.py --analyze-only   # reuse existing JSONL
```

Runs the real-block bench with `PARALLEL_TX_TIMING=1` and writes JSONL to `benchmarks/results/amdahl_timing.jsonl`, then prints achieved / theoretical / packing ceilings and wave efficiency (plot → `benchmarks/results/wave_efficiency.png`). Incomplete parallel samples (validate fail) impute shared serial phases from sequential.

---

## CPU profiling

```powershell
python .\profile_real_blocks.py
go tool pprof -http=localhost:8080 benchmarks\results\cpu_parallel.out
```

Writes `cpu_sequential.{out,txt}` and `cpu_parallel.{out,txt}` under `benchmarks/results/`. Edit `JSON_FILE` / `RUNS` in the script (≥100 for usable profiles).

---

## Fetch real blocks

Preferred (with parent `pre` alloc):

```powershell
python .\benchmarks\blocks\fetch_block_with_prestate.py
python .\benchmarks\blocks\fetch_block_with_prestate.py --block 25603091
```

Needs an RPC with `debug_traceBlockByNumber` + `prestateTracer` (`RPC_URI` at top of script). Writes `benchmarks/blocks/blocksdata/<n>.json`.

Legacy (txs only, empty genesis): `benchmarks/blocks/fetch_block.ps1` — fast but artificially cheap.
