# Parallel Geth — How to Run

Shared Go flags live in `core/parallel_flags.go`:


| Flag                                   | Role                                                     |
| -------------------------------------- | -------------------------------------------------------- |
| `ParallelTxGroupingByStorageOverlap`   | Wave grouping by declared address disjointness           |
| `ParallelTxWaveExecution`              | Concurrent execution within a wave                       |
| `ParallelTxDirectExecutionMaxWaveSize` | Waves ≤ N run on sequentially on shared state (no clone) |
| `ParallelTxDebug`                      | Access-list / grouping debug prints                      |
| `ParallelTxTiming`                     | Buffer Process() phase timings; print after run          |


---

## Correctness

```powershell
# Full tests package (long; raise timeout)
go test ./tests -count=1 -timeout 600m -v 2>&1 | Tee-Object run.log

# Blockchain / block tests only
go test ./tests -count=1 -timeout 600m -v -run TestBlockchain

# Single known-good block fixture
go test ./tests -count=1 -v -run "^TestBlockchain/ValidBlocks/bcValidBlockTest/SimpleTx3LowS.json$"

# Parallel-specific correctness
go test ./tests -count=1 -v -run "TestParallelVM"
```

Relevant tests: `tests/block_test.go`, `tests/parallel_vm_test.go`, `tests/parallel_vm_block_test.go`.

---



## Synthetic benchmarks

Test: `tests/parallel_execution_bench_test.go` → `TestParallelBenchmarkOutput`  
Scenarios: tx counts `{50,150,300,450,600}` × deps `{Isolated,Contended,Mixed}`.

```powershell
$env:BENCHMARK_OUTPUT_FILE="$PWD\optimization_logs\_temp_run.log"
$env:BRANCH_NAME="local"   # optional label in log lines
go test -v ./tests -count=1 -run TestParallelBenchmarkOutput -timeout 60m

# One scenario
go test -v ./tests -count=1 -run '^TestParallelBenchmarkOutput/600/Isolated$' -timeout 30m
```


| Env                     | Required | Notes                                    |
| ----------------------- | -------- | ---------------------------------------- |
| `BENCHMARK_OUTPUT_FILE` | yes      | Append-only results log (skips if unset) |
| `BRANCH_NAME`           | no       | Label in output lines                    |


---



## Real-block benchmarks

Script: `run_real_blocks_benchmark.py`  
Test: `benchmarks/paralell_bench_realBlocks_test.go` → `TestParallelBenchmarkAgainstRealBlocks`  
Block JSON dir: `benchmarks/blocks/blocksdata/`

```powershell
python .\run_real_blocks_benchmark.py
```



### Script constants (`run_real_blocks_benchmark.py`)


| Constant                            | Purpose                                                          |
| ----------------------------------- | ---------------------------------------------------------------- |
| `JSON_FILE`                         | Single file (e.g. `"25560620.json"`); `""` = all `.json` in dir  |
| `BENCHMARK_JSON_INPUT_DIR`          | Input directory                                                  |
| `BENCHMARK_OUTPUT_FILE_REAL_BLOCKS` | Results log path                                                 |
| `BRANCH_NAME`                       | Label in output lines                                            |
| `BENCHMARK_TIMEOUT`                 | `go test -timeout`                                               |
| `RUNS`                              | Repetitions per file                                             |
| `DIRECT_EXECUTION_MAX_WAVE_SIZES`   | `[]` = keep Go flag; `[4]` = one value; `list(range(9))` = sweep |
| `GOMAXPROCS`                        | Set to `"8"` in script                                           |




### Env passed to Go (set by the script)


| Env                                 | Notes                                         |
| ----------------------------------- | --------------------------------------------- |
| `BENCHMARK_OUTPUT_FILE_REAL_BLOCKS` | Required by test (skips if unset)             |
| `BENCHMARK_JSON_INPUT_DIR`          | JSON input dir (temp copy if `JSON_FILE` set) |
| `BENCHMARK_RUNS`                    | Sample count                                  |
| `BRANCH_NAME`                       | Label                                         |


---



## Fetch new real blocks

Script: `benchmarks/blocks/fetch_block.ps1`  
Writes: `benchmarks/blocks/blocksdata/<decimal_block_number>.json`

```powershell
cd benchmarks\blocks
.\fetch_block.ps1
```


| Setting      | Location                                       | Notes                                             |
| ------------ | ---------------------------------------------- | ------------------------------------------------- |
| `$rpcUri`    | top of script                                  | Ethereum JSON-RPC endpoint                        |
| Block number | hardcoded `"latest"` in `eth_getBlockByNumber` | Change to hex (e.g. `"0x…"`) for a specific block |


Fetches the block, builds EIP-2930 access lists via `eth_createAccessList`, emits the benchmark JSON schema.