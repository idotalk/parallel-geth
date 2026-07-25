// Copyright 2026 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package core

// ParallelTxGroupingByStorageOverlap controls grouping in Process and
// BuildTransactionStorageParallelGroups. Despite the name, grouping uses
// declared address disjointness (from, to, access-list addresses). When true
// (default), txs whose declared address sets are pairwise disjoint may share a
// wave. When false, each tx is its own group [[0],[1],...].
var ParallelTxGroupingByStorageOverlap = true

// ParallelTxWaveExecution runs txs in the same wave concurrently when true.
// Worker count is ParallelTxWorkers (each worker owns one reused StateDB fork).
// When false, txs in a wave still run strictly in ascending index order
// on the shared EVM — consensus-compatible with sequential Ethereum execution.
var ParallelTxWaveExecution = true

// ParallelTxWorkers is the max number of goroutines (and StateDB forks) inside a
// parallel wave. Each worker clones once from the wave-parent StateDB and reuses
// that dirty fork for later txs from the wave queue (safe under address-disjoint
// packing). If <= 0, runtime.GOMAXPROCS(0) is used.
var ParallelTxWorkers = 4

// ParallelTxDirectExecutionMaxWaveSize is the largest wave that executes
// directly on the shared StateDB, avoiding per-transaction state copies,
// goroutines, child EVMs, and merging. The default optimizes singleton waves.
var ParallelTxDirectExecutionMaxWaveSize = 9

// ParallelTxDebug enables debug logging for parallel transaction execution.
var ParallelTxDebug = false

// ParallelTxTiming buffers Process() phase timings (and validate/write via
// AttachPostProcessTiming). When PARALLEL_TX_TIMING_FILE is set from the bench,
// samples are also appended as JSONL for Amdahl analysis.
var ParallelTxTiming = false
