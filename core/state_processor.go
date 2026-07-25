// Copyright 2015 The go-ethereum Authors
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

import (
	"encoding/json"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/misc"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/tracing"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
	"github.com/holiman/uint256"
)

// StateProcessor is a basic Processor, which takes care of transitioning
// state from one point to another.
//
// StateProcessor implements Processor.
type StateProcessor struct {
	config *params.ChainConfig // Chain configuration options
	chain  *HeaderChain        // Canonical header chain
}

// NewStateProcessor initialises a new StateProcessor.
func NewStateProcessor(config *params.ChainConfig, chain *HeaderChain) *StateProcessor {
	return &StateProcessor{
		config: config,
		chain:  chain,
	}
}

// Process processes the state changes according to the Ethereum rules by running
// the transaction messages using the statedb and applying any rewards to both
// the processor (coinbase) and any included uncles.
//
// Process returns the receipts and logs accumulated during the process and
// returns the amount of gas that was used in the process. If any of the
// transactions failed to execute due to insufficient gas it will return an error.
func (p *StateProcessor) Process(block *types.Block, statedb *state.StateDB, cfg vm.Config) (*ProcessResult, error) {
	var (
		receipts    types.Receipts
		usedGas     = new(uint64)
		header      = block.Header()
		blockHash   = block.Hash()
		blockNumber = block.Number()
		allLogs     []*types.Log
		gp          = new(GasPool).AddGas(block.GasLimit())
	)

	// Mutate the block and state according to any hard-fork specs
	if p.config.DAOForkSupport && p.config.DAOForkBlock != nil && p.config.DAOForkBlock.Cmp(block.Number()) == 0 {
		misc.ApplyDAOHardFork(statedb)
	}
	var (
		context vm.BlockContext
		signer  = types.MakeSigner(p.config, header.Number, header.Time)
	)

	// Apply pre-execution system calls.
	var tracingStateDB = vm.StateDB(statedb)
	if hooks := cfg.Tracer; hooks != nil {
		tracingStateDB = state.NewHookedState(statedb, hooks)
	}
	context = NewEVMBlockContext(header, p.chain, nil)
	evm := vm.NewEVM(context, tracingStateDB, p.config, cfg)

	if beaconRoot := block.BeaconRoot(); beaconRoot != nil {
		ProcessBeaconBlockRoot(*beaconRoot, evm)
	}
	if p.config.IsPrague(block.Number(), block.Time()) || p.config.IsVerkle(block.Number(), block.Time()) {
		ProcessParentBlockHash(block.ParentHash(), evm)
	}

	// Print declared access (EIP-2930) for each transaction before processing.
	if ParallelTxDebug && ParallelTxWaveExecution {
		PrintTransactionAccessLists(block.Transactions())
		PrintTransactionAccessOverlapMatrix(block.Transactions())
		PrintTransactionStorageAccessOverlapMatrix(block.Transactions())
		PrintTransactionStorageParallelGroups(block.Transactions(), signer)
	}

	// --- Fork: waves from BuildTransactionStorageParallelGroups (address-disjoint).
	// Waves run sequentially on the live StateDB. Within a wave:
	// - ParallelTxWaveExecution false: apply txs in ascending index order (canonical).
	// - ParallelTxWaveExecution true: ParallelTxWorkers goroutines each clone once
	//   and reuse that dirty fork for queued wave txs; receipts stored by index.
	txs := block.Transactions()
	n := len(txs)
	receipts = make([]*types.Receipt, n)

	var groupingWall time.Duration
	groupingStart := time.Now()
	groups, err := BuildTransactionStorageParallelGroups(txs, signer)
	if err != nil {
		return nil, fmt.Errorf("build tx parallel groups: %w", err)
	}
	if ParallelTxTiming {
		groupingWall = time.Since(groupingStart)
	}

	var waveTimings []wavePhaseTiming
	var txApply []time.Duration
	workersUsed := 0
	if ParallelTxTiming {
		waveTimings = make([]wavePhaseTiming, 0, len(groups))
		txApply = make([]time.Duration, n)
	}

	for groupIdx, group := range groups {
		var waveStart time.Time
		if ParallelTxTiming {
			waveStart = time.Now()
		}
		sortedIdx := append([]int(nil), group...)
		sort.Ints(sortedIdx)
		waveTiming := wavePhaseTiming{txCount: len(sortedIdx), txIndices: append([]int(nil), sortedIdx...)}

		if len(sortedIdx) <= ParallelTxDirectExecutionMaxWaveSize || !ParallelTxWaveExecution {
			for _, i := range sortedIdx {
				tx := txs[i]
				var txStart time.Time
				if ParallelTxTiming {
					txStart = time.Now()
				}
				msg, err := TransactionToMessage(tx, signer, header.BaseFee)
				if err != nil {
					return nil, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
				}
				statedb.SetTxContext(tx.Hash(), i)
				var txGasUsed uint64
				receipt, err := ApplyTransactionWithEVM(msg, gp, statedb, blockNumber, blockHash, context.Time, tx, &txGasUsed, evm)
				if err != nil {
					return nil, fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
				}
				receipts[i] = receipt
				if ParallelTxTiming {
					txApply[i] = time.Since(txStart)
				}
			}
			if ParallelTxDebug {
				fmt.Printf("finished transaction execution group %d (tx indices, sequential in-wave): %v\n", groupIdx, sortedIdx)
			}
			if ParallelTxTiming {
				waveTiming.wall = time.Since(waveStart)
				waveTimings = append(waveTimings, waveTiming)
			}
			continue
		}

		waveTiming.parallel = true
		coinbaseBase := new(uint256.Int).Set(statedb.GetBalance(header.Coinbase))
		coinbaseFinal := new(uint256.Int).Set(coinbaseBase)
		txForks := make([]*state.StateDB, n)
		workers := ParallelTxWorkers
		if workers <= 0 {
			workers = runtime.GOMAXPROCS(0)
		}
		if workers > len(sortedIdx) {
			workers = len(sortedIdx)
		}
		if workers > workersUsed {
			workersUsed = workers
		}
		jobs := make(chan int, len(sortedIdx))
		for _, idx := range sortedIdx {
			jobs <- idx
		}
		close(jobs)

		var wg sync.WaitGroup
		var errMu sync.Mutex
		var firstErr error
		var concurrentStart time.Time
		if ParallelTxTiming {
			concurrentStart = time.Now()
		}
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				child := statedb.CopyForParallelTx()

				for i := range jobs {
					tx := txs[i]
					var txStart time.Time
					if ParallelTxTiming {
						txStart = time.Now()
					}
					msg, err := TransactionToMessage(tx, signer, header.BaseFee)
					if err != nil {
						errMu.Lock()
						if firstErr == nil {
							firstErr = fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
						}
						errMu.Unlock()
						return
					}

					var parallelStateDB vm.StateDB = child
					if hooks := cfg.Tracer; hooks != nil {
						parallelStateDB = state.NewHookedState(child, hooks)
					}
					parallelEVM := vm.NewEVM(context, parallelStateDB, p.config, cfg)
					child.SetTxContext(tx.Hash(), i)

					var txGasUsed uint64
					receipt, err := ApplyTransactionWithEVM(msg, gp, child, blockNumber, blockHash, context.Time, tx, &txGasUsed, parallelEVM)
					if ParallelTxTiming {
						txApply[i] = time.Since(txStart)
					}
					if err != nil {
						errMu.Lock()
						if firstErr == nil {
							firstErr = fmt.Errorf("could not apply tx %d [%v]: %w", i, tx.Hash().Hex(), err)
						}
						errMu.Unlock()
						return
					}
					receipts[i] = receipt
					txForks[i] = child
				}
			}()
		}
		wg.Wait()
		if ParallelTxTiming {
			waveTiming.concurrentWall = time.Since(concurrentStart)
		}
		if firstErr != nil {
			return nil, firstErr
		}
		var mergeStart time.Time
		if ParallelTxTiming {
			mergeStart = time.Now()
		}
		// Accounts once per reused fork; logs in ascending tx index for canonical indices.
		mergedForks := make(map[*state.StateDB]struct{}, workers)
		for _, i := range sortedIdx {
			child := txForks[i]
			if _, ok := mergedForks[child]; ok {
				continue
			}
			mergedForks[child] = struct{}{}
			childCoinbase := child.GetBalance(header.Coinbase)
			if childCoinbase.Cmp(coinbaseBase) >= 0 {
				coinbaseFinal.Add(coinbaseFinal, new(uint256.Int).Sub(childCoinbase, coinbaseBase))
			} else {
				coinbaseFinal.Sub(coinbaseFinal, new(uint256.Int).Sub(coinbaseBase, childCoinbase))
			}
			statedb.MergeParallelChildAccounts(child)
		}
		for _, i := range sortedIdx {
			statedb.MergeParallelChildLogs(txForks[i], txs[i].Hash())
		}
		// Coinbase balance deltas are additive; dynamic reads
		// conflicts still require observed read/write sets and speculative re-execution and are not handled.
		statedb.SetBalance(header.Coinbase, coinbaseFinal, tracing.BalanceIncreaseRewardTransactionFee)
		if err := statedb.Error(); err != nil {
			return nil, err
		}
		if ParallelTxTiming {
			waveTiming.mergeWall = time.Since(mergeStart)
		}
		if ParallelTxDebug {
			fmt.Printf("finished transaction execution group %d (tx indices, concurrent in-wave): %v\n", groupIdx, sortedIdx)
		}
		if ParallelTxTiming {
			waveTiming.wall = time.Since(waveStart)
			waveTimings = append(waveTimings, waveTiming)
		}
	}

	finalStart := time.Now()
	normalizeReceiptCumulativeGas(receipts)
	if n > 0 {
		*usedGas = receipts[n-1].CumulativeGasUsed
	}
	for i := 0; i < n; i++ {
		allLogs = append(allLogs, receipts[i].Logs...)
	}
	// Read requests if Prague is enabled.
	var requests [][]byte
	if p.config.IsPrague(block.Number(), block.Time()) {
		requests = [][]byte{}
		// EIP-6110
		if err := ParseDepositLogs(&requests, allLogs, p.config); err != nil {
			return nil, err
		}
		// EIP-7002
		if err := ProcessWithdrawalQueue(&requests, evm); err != nil {
			return nil, err
		}
		// EIP-7251
		if err := ProcessConsolidationQueue(&requests, evm); err != nil {
			return nil, err
		}
	}

	// Finalize the block, applying any consensus engine specific extras (e.g. block rewards)
	p.chain.engine.Finalize(p.chain, header, tracingStateDB, block.Body())
	if ParallelTxTiming && n > 0 {
		var mergeTotal time.Duration
		for _, w := range waveTimings {
			mergeTotal += w.mergeWall
		}
		if workersUsed == 0 && ParallelTxGroupingByStorageOverlap && ParallelTxWaveExecution {
			workersUsed = ParallelTxWorkers
			if workersUsed <= 0 {
				workersUsed = runtime.GOMAXPROCS(0)
			}
		}
		recordProcessWallTiming(processTimingRecord{
			grouping:     groupingWall,
			waves:        waveTimings,
			finalization: time.Since(finalStart),
			txCount:      n,
			groupCount:   len(groups),
			parallel:     ParallelTxGroupingByStorageOverlap && ParallelTxWaveExecution,
			txApply:      txApply,
			workers:      workersUsed,
			mergeTotal:   mergeTotal,
		})
	}

	return &ProcessResult{
		Receipts: receipts,
		Requests: requests,
		Logs:     allLogs,
		GasUsed:  *usedGas,
	}, nil
}

type wavePhaseTiming struct {
	txCount        int
	txIndices      []int
	wall           time.Duration
	concurrentWall time.Duration // wall for parallel goroutine phase
	mergeWall      time.Duration // wall for sequential merge phase
	parallel       bool
}

type processTimingRecord struct {
	grouping     time.Duration
	waves        []wavePhaseTiming
	finalization time.Duration
	validate     time.Duration
	write        time.Duration
	mergeTotal   time.Duration
	txCount      int
	groupCount   int
	workers      int
	parallel     bool
	complete     bool            // true if validate+write finished successfully
	txApply      []time.Duration // per-tx apply wall (seq costs for Amdahl)
}

var (
	processTimingMu      sync.Mutex
	processTimingRecords []processTimingRecord
	parallelTxTimingFile string
)

// SetParallelTxTimingFile sets the JSONL path flushed by PrintAndClearParallelTxTimings.
// Empty disables file output (stdout summary still prints).
func SetParallelTxTimingFile(path string) {
	processTimingMu.Lock()
	parallelTxTimingFile = path
	processTimingMu.Unlock()
}

func recordProcessWallTiming(record processTimingRecord) {
	processTimingMu.Lock()
	processTimingRecords = append(processTimingRecords, record)
	processTimingMu.Unlock()
}

// AttachPostProcessTiming adds validate+write walls to the latest Process timing sample.
// complete is true only when the block was fully written after a successful validate.
func AttachPostProcessTiming(validate, write time.Duration, complete bool) {
	if !ParallelTxTiming {
		return
	}
	processTimingMu.Lock()
	defer processTimingMu.Unlock()
	if n := len(processTimingRecords); n > 0 {
		processTimingRecords[n-1].validate = validate
		processTimingRecords[n-1].write = write
		processTimingRecords[n-1].complete = complete
	}
}

// ClearParallelTxTimings discards all buffered timing diagnostics.
func ClearParallelTxTimings() {
	processTimingMu.Lock()
	processTimingRecords = nil
	processTimingMu.Unlock()
}

// amdahlTimingJSON is one Process sample for offline Amdahl analysis.
type amdahlTimingJSON struct {
	Mode             string           `json:"mode"`
	Complete         bool             `json:"complete"`
	TxCount          int              `json:"tx_count"`
	GroupCount       int              `json:"group_count"`
	Workers          int              `json:"workers"`
	GroupingNs       int64            `json:"grouping_ns"`
	FinalizationNs   int64            `json:"finalization_ns"`
	ValidateNs       int64            `json:"validate_ns"`
	WriteNs          int64            `json:"write_ns"`
	MergeNs          int64            `json:"merge_ns"`
	SerialNs         int64            `json:"serial_ns"`
	ParallelizableNs int64            `json:"parallelizable_ns"`
	TxApplyNs        []int64          `json:"tx_apply_ns"`
	Waves            []amdahlWaveJSON `json:"waves"`
}

type amdahlWaveJSON struct {
	Txs              []int `json:"txs"`
	Size             int   `json:"size"`
	WallNs           int64 `json:"wall_ns"`
	ConcurrentWallNs int64 `json:"concurrent_wall_ns"`
	MergeNs          int64 `json:"merge_ns"`
	Parallel         bool  `json:"parallel"`
}

func recordToAmdahlJSON(record processTimingRecord) amdahlTimingJSON {
	mode := "sequential"
	if record.parallel {
		mode = "parallel"
	}
	txApplyNs := make([]int64, len(record.txApply))
	for i, d := range record.txApply {
		txApplyNs[i] = d.Nanoseconds()
	}
	waves := make([]amdahlWaveJSON, len(record.waves))
	var parallelizable time.Duration
	for i, w := range record.waves {
		waves[i] = amdahlWaveJSON{
			Txs:              append([]int(nil), w.txIndices...),
			Size:             w.txCount,
			WallNs:           w.wall.Nanoseconds(),
			ConcurrentWallNs: w.concurrentWall.Nanoseconds(),
			MergeNs:          w.mergeWall.Nanoseconds(),
			Parallel:         w.parallel,
		}
	}
	if record.parallel {
		// Observed parallelizable wall: concurrent span per parallel wave,
		// full wave wall for direct-exec waves (ideally parallelizable).
		for _, w := range record.waves {
			if w.parallel {
				parallelizable += w.concurrentWall
			} else {
				parallelizable += w.wall
			}
		}
	} else {
		for _, d := range record.txApply {
			parallelizable += d
		}
	}
	serial := record.grouping + record.mergeTotal + record.finalization + record.validate + record.write
	return amdahlTimingJSON{
		Mode:             mode,
		Complete:         record.complete,
		TxCount:          record.txCount,
		GroupCount:       record.groupCount,
		Workers:          record.workers,
		GroupingNs:       record.grouping.Nanoseconds(),
		FinalizationNs:   record.finalization.Nanoseconds(),
		ValidateNs:       record.validate.Nanoseconds(),
		WriteNs:          record.write.Nanoseconds(),
		MergeNs:          record.mergeTotal.Nanoseconds(),
		SerialNs:         serial.Nanoseconds(),
		ParallelizableNs: parallelizable.Nanoseconds(),
		TxApplyNs:        txApplyNs,
		Waves:            waves,
	}
}

// PrintAndClearParallelTxTimings appends JSONL if configured and clears the buffer.
func PrintAndClearParallelTxTimings() {
	processTimingMu.Lock()
	records := processTimingRecords
	processTimingRecords = nil
	outPath := parallelTxTimingFile
	processTimingMu.Unlock()

	if outPath != "" && len(records) > 0 {
		if err := appendAmdahlTimingJSONL(outPath, records); err != nil {
			fmt.Printf("parallel timing JSONL write failed: %v\n", err)
		} else {
			fmt.Printf("parallel timing JSONL appended %d sample(s) -> %s\n", len(records), outPath)
		}
	}
}

func appendAmdahlTimingJSONL(path string, records []processTimingRecord) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, record := range records {
		if err := enc.Encode(recordToAmdahlJSON(record)); err != nil {
			return err
		}
	}
	return nil
}

// ApplyTransactionWithEVM attempts to apply a transaction to the given state database
// and uses the input parameters for its environment similar to ApplyTransaction. However,
// this method takes an already created EVM instance as input.
func ApplyTransactionWithEVM(msg *Message, gp *GasPool, statedb *state.StateDB, blockNumber *big.Int, blockHash common.Hash, blockTime uint64, tx *types.Transaction, usedGas *uint64, evm *vm.EVM) (receipt *types.Receipt, err error) {
	if hooks := evm.Config.Tracer; hooks != nil {
		if hooks.OnTxStart != nil {
			hooks.OnTxStart(evm.GetVMContext(), tx, msg.From)
		}
		if hooks.OnTxEnd != nil {
			defer func() { hooks.OnTxEnd(receipt, err) }()
		}
	}
	// Apply the transaction to the current state (included in the env).
	result, err := ApplyMessage(evm, msg, gp)
	if err != nil {
		return nil, err
	}
	// Update the state with pending changes.
	var root []byte
	if evm.ChainConfig().IsByzantium(blockNumber) {
		evm.StateDB.Finalise(true)
	} else {
		root = statedb.IntermediateRoot(evm.ChainConfig().IsEIP158(blockNumber)).Bytes()
	}
	*usedGas += result.UsedGas

	// Merge the tx-local access event into the "block-local" one, in order to collect
	// all values, so that the witness can be built.
	if statedb.Database().TrieDB().IsVerkle() {
		statedb.AccessEvents().Merge(evm.AccessEvents)
	}
	return MakeReceipt(evm, result, statedb, blockNumber, blockHash, blockTime, tx, *usedGas, root), nil
}

// MakeReceipt generates the receipt object for a transaction given its execution result.
func MakeReceipt(evm *vm.EVM, result *ExecutionResult, statedb *state.StateDB, blockNumber *big.Int, blockHash common.Hash, blockTime uint64, tx *types.Transaction, usedGas uint64, root []byte) *types.Receipt {
	// Create a new receipt for the transaction, storing the intermediate root and gas used
	// by the tx.
	receipt := &types.Receipt{Type: tx.Type(), PostState: root, CumulativeGasUsed: usedGas}
	if result.Failed() {
		receipt.Status = types.ReceiptStatusFailed
	} else {
		receipt.Status = types.ReceiptStatusSuccessful
	}
	receipt.TxHash = tx.Hash()
	receipt.GasUsed = result.UsedGas

	if tx.Type() == types.BlobTxType {
		receipt.BlobGasUsed = uint64(len(tx.BlobHashes()) * params.BlobTxBlobGasPerBlob)
		receipt.BlobGasPrice = evm.Context.BlobBaseFee
	}

	// If the transaction created a contract, store the creation address in the receipt.
	if tx.To() == nil {
		receipt.ContractAddress = crypto.CreateAddress(evm.TxContext.Origin, tx.Nonce())
	}

	// Set the receipt logs and create the bloom filter.
	receipt.Logs = statedb.GetLogs(tx.Hash(), blockNumber.Uint64(), blockHash, blockTime)
	receipt.Bloom = types.CreateBloom(receipt)
	receipt.BlockHash = blockHash
	receipt.BlockNumber = blockNumber
	receipt.TransactionIndex = uint(statedb.TxIndex())
	return receipt
}

// ApplyTransaction attempts to apply a transaction to the given state database
// and uses the input parameters for its environment. It returns the receipt
// for the transaction, gas used and an error if the transaction failed,
// indicating the block was invalid.
func ApplyTransaction(evm *vm.EVM, gp *GasPool, statedb *state.StateDB, header *types.Header, tx *types.Transaction, usedGas *uint64) (*types.Receipt, error) {
	msg, err := TransactionToMessage(tx, types.MakeSigner(evm.ChainConfig(), header.Number, header.Time), header.BaseFee)
	if err != nil {
		return nil, err
	}
	// Create a new context to be used in the EVM environment
	return ApplyTransactionWithEVM(msg, gp, statedb, header.Number, header.Hash(), header.Time, tx, usedGas, evm)
}

// ProcessBeaconBlockRoot applies the EIP-4788 system call to the beacon block root
// contract. This method is exported to be used in tests.
func ProcessBeaconBlockRoot(beaconRoot common.Hash, evm *vm.EVM) {
	if tracer := evm.Config.Tracer; tracer != nil {
		onSystemCallStart(tracer, evm.GetVMContext())
		if tracer.OnSystemCallEnd != nil {
			defer tracer.OnSystemCallEnd()
		}
	}
	msg := &Message{
		From:      params.SystemAddress,
		GasLimit:  30_000_000,
		GasPrice:  common.Big0,
		GasFeeCap: common.Big0,
		GasTipCap: common.Big0,
		To:        &params.BeaconRootsAddress,
		Data:      beaconRoot[:],
	}
	evm.SetTxContext(NewEVMTxContext(msg))
	evm.StateDB.AddAddressToAccessList(params.BeaconRootsAddress)
	_, _, _ = evm.Call(msg.From, *msg.To, msg.Data, 30_000_000, common.U2560)
	evm.StateDB.Finalise(true)
}

// ProcessParentBlockHash stores the parent block hash in the history storage contract
// as per EIP-2935/7709.
func ProcessParentBlockHash(prevHash common.Hash, evm *vm.EVM) {
	if tracer := evm.Config.Tracer; tracer != nil {
		onSystemCallStart(tracer, evm.GetVMContext())
		if tracer.OnSystemCallEnd != nil {
			defer tracer.OnSystemCallEnd()
		}
	}
	msg := &Message{
		From:      params.SystemAddress,
		GasLimit:  30_000_000,
		GasPrice:  common.Big0,
		GasFeeCap: common.Big0,
		GasTipCap: common.Big0,
		To:        &params.HistoryStorageAddress,
		Data:      prevHash.Bytes(),
	}
	evm.SetTxContext(NewEVMTxContext(msg))
	evm.StateDB.AddAddressToAccessList(params.HistoryStorageAddress)
	_, _, err := evm.Call(msg.From, *msg.To, msg.Data, 30_000_000, common.U2560)
	if err != nil {
		panic(err)
	}
	if evm.StateDB.AccessEvents() != nil {
		evm.StateDB.AccessEvents().Merge(evm.AccessEvents)
	}
	evm.StateDB.Finalise(true)
}

// ProcessWithdrawalQueue calls the EIP-7002 withdrawal queue contract.
// It returns the opaque request data returned by the contract.
func ProcessWithdrawalQueue(requests *[][]byte, evm *vm.EVM) error {
	return processRequestsSystemCall(requests, evm, 0x01, params.WithdrawalQueueAddress)
}

// ProcessConsolidationQueue calls the EIP-7251 consolidation queue contract.
// It returns the opaque request data returned by the contract.
func ProcessConsolidationQueue(requests *[][]byte, evm *vm.EVM) error {
	return processRequestsSystemCall(requests, evm, 0x02, params.ConsolidationQueueAddress)
}

func processRequestsSystemCall(requests *[][]byte, evm *vm.EVM, requestType byte, addr common.Address) error {
	if tracer := evm.Config.Tracer; tracer != nil {
		onSystemCallStart(tracer, evm.GetVMContext())
		if tracer.OnSystemCallEnd != nil {
			defer tracer.OnSystemCallEnd()
		}
	}
	msg := &Message{
		From:      params.SystemAddress,
		GasLimit:  30_000_000,
		GasPrice:  common.Big0,
		GasFeeCap: common.Big0,
		GasTipCap: common.Big0,
		To:        &addr,
	}
	evm.SetTxContext(NewEVMTxContext(msg))
	evm.StateDB.AddAddressToAccessList(addr)
	ret, _, err := evm.Call(msg.From, *msg.To, msg.Data, 30_000_000, common.U2560)
	evm.StateDB.Finalise(true)
	if err != nil {
		return fmt.Errorf("system call failed to execute: %v", err)
	}
	if len(ret) == 0 {
		return nil // skip empty output
	}
	// Append prefixed requestsData to the requests list.
	requestsData := make([]byte, len(ret)+1)
	requestsData[0] = requestType
	copy(requestsData[1:], ret)
	*requests = append(*requests, requestsData)
	return nil
}

var depositTopic = common.HexToHash("0x649bbc62d0e31342afea4e5cd82d4049e7e1ee912fc0889aa790803be39038c5")

// ParseDepositLogs extracts the EIP-6110 deposit values from logs emitted by
// BeaconDepositContract.
func ParseDepositLogs(requests *[][]byte, logs []*types.Log, config *params.ChainConfig) error {
	deposits := make([]byte, 1) // note: first byte is 0x00 (== deposit request type)
	for _, log := range logs {
		if log.Address == config.DepositContractAddress && len(log.Topics) > 0 && log.Topics[0] == depositTopic {
			request, err := types.DepositLogToRequest(log.Data)
			if err != nil {
				return fmt.Errorf("unable to parse deposit data: %v", err)
			}
			deposits = append(deposits, request...)
		}
	}
	if len(deposits) > 1 {
		*requests = append(*requests, deposits)
	}
	return nil
}

func onSystemCallStart(tracer *tracing.Hooks, ctx *tracing.VMContext) {
	if tracer.OnSystemCallStartV2 != nil {
		tracer.OnSystemCallStartV2(ctx)
	} else if tracer.OnSystemCallStart != nil {
		tracer.OnSystemCallStart()
	}
}
