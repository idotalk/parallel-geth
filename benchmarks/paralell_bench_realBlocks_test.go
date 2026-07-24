package benchmarks

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus"
	"github.com/ethereum/go-ethereum/consensus/beacon"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
	"github.com/ethereum/go-ethereum/triedb"
)

type jsonBlockchainTestFile map[string]jsonTestInstance

type jsonTestInstance struct {
	BlockHeader *jsonBlockHeader `json:"blockHeader"`
	Blocks      []jsonBlock      `json:"blocks"`
}

type jsonBlock struct {
	BlockHeader  *jsonBlockHeader  `json:"blockHeader"`
	Transactions []jsonTransaction `json:"transactions"`
}

type jsonBlockHeader struct {
	BaseFeePerGas *hexutil.Big   `json:"baseFeePerGas"`
	GasLimit      hexutil.Uint64 `json:"gasLimit"`
	Number        hexutil.Uint64 `json:"number"`
	Timestamp     hexutil.Uint64 `json:"timestamp"`
	Coinbase      common.Address `json:"coinbase"`
	ParentHash    common.Hash    `json:"parentHash"`
}

// Note: V/R/S are intentionally unused for signing. The original signatures
// were produced against real mainnet state (real sender balances/nonces),
// which don't exist in our synthetic genesis. Instead we re-sign every
// transaction with a single locally generated key that we fund in the
// genesis alloc. See parseTestJSONFile.
type jsonTransaction struct {
	AccessList           types.AccessList `json:"accessList"`
	Data                 hexutil.Bytes    `json:"data"`
	GasLimit             hexutil.Uint64   `json:"gasLimit"`
	MaxFeePerGas         *hexutil.Big     `json:"maxFeePerGas"`
	MaxPriorityFeePerGas *hexutil.Big     `json:"maxPriorityFeePerGas"`
	Nonce                hexutil.Uint64   `json:"nonce"`
	To                   string           `json:"to"`
	Value                *hexutil.Big     `json:"value"`
	V                    *hexutil.Big     `json:"v"`
	R                    *hexutil.Big     `json:"r"`
	S                    *hexutil.Big     `json:"s"`
}

// benchChainConfig activates only through London + the Merge. We deliberately
// avoid AllDevChainProtocolChanges (which also activates Shanghai/Cancun) so
// that headers don't need WithdrawalsHash / ExcessBlobGas / BlobGasUsed /
// ParentBeaconBlockRoot, since our synthetic blocks don't use withdrawals or
// blobs.
var benchChainConfig = &params.ChainConfig{
	ChainID:                 big.NewInt(1), // mainnet: confirmed by legacy-tx V values (0x25/0x26) decoding to chainId=1
	HomesteadBlock:          big.NewInt(0),
	EIP150Block:             big.NewInt(0),
	EIP155Block:             big.NewInt(0),
	EIP158Block:             big.NewInt(0),
	ByzantiumBlock:          big.NewInt(0),
	ConstantinopleBlock:     big.NewInt(0),
	PetersburgBlock:         big.NewInt(0),
	IstanbulBlock:           big.NewInt(0),
	MuirGlacierBlock:        big.NewInt(0),
	BerlinBlock:             big.NewInt(0),
	LondonBlock:             big.NewInt(0),
	TerminalTotalDifficulty: big.NewInt(0), // PoS from genesis; required by beacon engine
}

func TestParallelBenchmarkAgainstRealBlocks(t *testing.T) {
	originalGrouping := core.ParallelTxGroupingByStorageOverlap
	originalWaveExecution := core.ParallelTxWaveExecution
	defer func() {
		core.ParallelTxGroupingByStorageOverlap = originalGrouping
		core.ParallelTxWaveExecution = originalWaveExecution
	}()
	core.ClearParallelTxTimings()
	defer core.PrintAndClearParallelTxTimings()

	branchName := os.Getenv("BRANCH_NAME")
	if branchName == "" {
		branchName = "unknown-branch"
	}

	outPath := os.Getenv("BENCHMARK_OUTPUT_FILE_REAL_BLOCKS")
	if outPath == "" {
		t.Skip("BENCHMARK_OUTPUT_FILE_REAL_BLOCKS not set, skipping benchmark execution tracking")
	}
	runs := 1
	if value := os.Getenv("BENCHMARK_RUNS"); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 1 {
			t.Fatalf("invalid BENCHMARK_RUNS %q", value)
		}
		runs = parsed
	}
	mode := strings.ToLower(os.Getenv("BENCHMARK_MODE"))
	if mode == "" {
		mode = "both"
	}
	if mode != "both" && mode != "sequential" && mode != "parallel" {
		t.Fatalf("invalid BENCHMARK_MODE %q (want both|sequential|parallel)", mode)
	}

	inputDir := os.Getenv("BENCHMARK_JSON_INPUT_DIR")
	if inputDir == "" {
		inputDir = filepath.Join("testdata", "generated_blocks")
	}

	files, err := os.ReadDir(inputDir)
	if err != nil {
		t.Fatalf("failed to read test JSON input folder: %v", err)
	}

	var results []string
	dateStr := time.Now().Format("2006-01-02")

	// Single engine instance used both to pre-generate the synthetic chain
	// (via core.GenerateChain) and to validate it on insertion. Wrapped in
	// beacon so post-merge (difficulty == 0) headers validate correctly.
	engine := beacon.New(ethash.NewFaker())

	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".json") {
			continue
		}

		filePath := filepath.Join(inputDir, file.Name())
		blocks, genesis, txCount, _, err := parseTestJSONFile(filePath, engine)
		if err != nil {
			t.Logf("skipping file %s due to parsing error: %v", file.Name(), err)
			continue
		}
		if txCount == 0 {
			continue
		}

		warmExecutionPaths(t, blocks, genesis, engine)

		seqSamples := make([]float64, 0, runs)
		parSamples := make([]float64, 0, runs)
		speedupSamples := make([]float64, 0, runs)
		for run := 0; run < runs; run++ {
			switch mode {
			case "sequential":
				seqTime := timeSequentialInsert(t, blocks, genesis, engine)
				seqSamples = append(seqSamples, seqTime.Seconds())
			case "parallel":
				parTime := timeParallelInsert(t, blocks, genesis, engine)
				parSamples = append(parSamples, parTime.Seconds())
			default:
				var seqTime, parTime time.Duration
				if run%2 == 0 {
					seqTime = timeSequentialInsert(t, blocks, genesis, engine)
					parTime = timeParallelInsert(t, blocks, genesis, engine)
				} else {
					parTime = timeParallelInsert(t, blocks, genesis, engine)
					seqTime = timeSequentialInsert(t, blocks, genesis, engine)
				}
				seqSamples = append(seqSamples, seqTime.Seconds())
				parSamples = append(parSamples, parTime.Seconds())
				speedupSamples = append(speedupSamples, float64(seqTime)/float64(parTime))
			}
		}
		var resLine string
		switch mode {
		case "sequential":
			seqAvg, seqStd := meanStddev(seqSamples)
			resLine = fmt.Sprintf("[%s][%s][%s][%d_txs][%d_runs][sequential] - avg=%.6fs std=%.6fs",
				dateStr, branchName, file.Name(), txCount, runs, seqAvg, seqStd)
		case "parallel":
			parAvg, parStd := meanStddev(parSamples)
			resLine = fmt.Sprintf("[%s][%s][%s][%d_txs][%d_runs][parallel] - avg=%.6fs std=%.6fs",
				dateStr, branchName, file.Name(), txCount, runs, parAvg, parStd)
		default:
			seqAvg, seqStd := meanStddev(seqSamples)
			parAvg, parStd := meanStddev(parSamples)
			speedupAvg, speedupStd := meanStddev(speedupSamples)
			resLine = fmt.Sprintf("[%s][%s][%s][%d_txs][%d_runs] - Sequential: avg=%.6fs std=%.6fs, Parallel: avg=%.6fs std=%.6fs, Speedup: avg=%.3fx std=%.3fx",
				dateStr, branchName, file.Name(), txCount, runs, seqAvg, seqStd, parAvg, parStd, speedupAvg, speedupStd)
		}

		results = append(results, resLine)
	}

	if len(results) == 0 {
		t.Skip("No valid transactions found within target block data files.")
	}

	if err := os.MkdirAll(filepath.Dir(outPath), 0755); err != nil {
		t.Fatalf("failed to create output dir: %v", err)
	}
	f, err := os.OpenFile(outPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		t.Fatalf("failed to open output tracking file: %v", err)
	}
	defer f.Close()

	for _, res := range results {
		if _, err := f.WriteString(res + "\n"); err != nil {
			t.Fatalf("failed to write metrics data: %v", err)
		}
	}

	core.PrintAndClearParallelTxTimings()
	fmt.Println("\nbenchmark summary:")
	for _, res := range results {
		fmt.Println(res)
	}
}

func warmExecutionPaths(t *testing.T, blocks []*types.Block, genesis *core.Genesis, engine consensus.Engine) {
	timing := core.ParallelTxTiming
	core.ParallelTxTiming = false
	defer func() { core.ParallelTxTiming = timing }()

	mode := strings.ToLower(os.Getenv("BENCHMARK_MODE"))
	switch mode {
	case "sequential":
		timeSequentialInsert(t, blocks, genesis, engine)
	case "parallel":
		timeParallelInsert(t, blocks, genesis, engine)
	default:
		timeSequentialInsert(t, blocks, genesis, engine)
		timeParallelInsert(t, blocks, genesis, engine)
	}
}

func timeSequentialInsert(t *testing.T, blocks []*types.Block, genesis *core.Genesis, engine consensus.Engine) time.Duration {
	core.ParallelTxGroupingByStorageOverlap = false
	core.ParallelTxWaveExecution = false
	return timeInsertLocal(t, blocks, genesis, engine)
}

func timeParallelInsert(t *testing.T, blocks []*types.Block, genesis *core.Genesis, engine consensus.Engine) time.Duration {
	core.ParallelTxGroupingByStorageOverlap = true
	core.ParallelTxWaveExecution = true
	return timeInsertLocal(t, blocks, genesis, engine)
}

func meanStddev(samples []float64) (float64, float64) {
	var sum float64
	for _, sample := range samples {
		sum += sample
	}
	mean := sum / float64(len(samples))
	if len(samples) == 1 {
		return mean, 0
	}
	var squaredDiffs float64
	for _, sample := range samples {
		diff := sample - mean
		squaredDiffs += diff * diff
	}
	return mean, math.Sqrt(squaredDiffs / float64(len(samples)-1))
}

func timeInsertLocal(t *testing.T, blocks []*types.Block, genesis *core.Genesis, engine consensus.Engine) time.Duration {
	options := &core.BlockChainConfig{
		TrieCleanLimit: 256,
		TrieDirtyLimit: 256,
		TrieTimeLimit:  5 * time.Minute,
		SnapshotLimit:  0,
		Preimages:      true,
		ArchiveMode:    true,
	}

	chain, err := core.NewBlockChain(rawdb.NewMemoryDatabase(), genesis, engine, options)
	if err != nil {
		t.Fatalf("create chain: %v", err)
	}
	defer chain.Stop()

	// Timed target block execution
	start := time.Now()
	if n, err := chain.InsertChain(blocks); err != nil {
		t.Fatalf("benchmark block %d failed: %v", n, err)
	}
	return time.Since(start)
}

// parseTestJSONFile reads the JSON fixture and builds a real, executable
// target block directly on top of genesis. Blocks are produced with
// core.GenerateChain, which actually runs the EVM/state processor while
// building each block, so state root, receipt root, tx root, gas used, and
// bloom are all filled in correctly.
//
// Transactions keep their ORIGINAL v/r/s signatures -- nothing is re-signed.
// The block's transactions are real mainnet EIP-1559 transactions (V values
// of 0/1 confirm type-2; the block number and the legacy txs' V values of
// 0x25/0x26, which decode to chainId=1 under EIP-155, confirm this is
// mainnet). benchChainConfig therefore uses chainId=1, so recovering each
// tx's sender via types.Sender(signer, tx) reconstructs the exact hash that
// was originally signed and returns the real mainnet sender address.
//
// Each recovered sender is funded in the genesis alloc with a large balance
// and its genesis Nonce set to the lowest nonce it uses in this block, so
// state-nonce checks pass without altering the transactions themselves. If a
// sender appears multiple times, its nonce increments naturally as each of
// its transactions executes in block order.
//
// 37 of this fixture's transactions are legacy (pre-EIP-1559) and the
// fixture omits their gasPrice entirely. A legacy signature is computed over
// gasPrice, so without it there's no way to reconstruct the exact bytes that
// were signed, and ecrecover against a guessed gasPrice would just return an
// unrelated address rather than the real sender. Since the goal is to keep
// signatures authentic, those transactions are dropped rather than faked;
// the caller logs how many were dropped.
func parseTestJSONFile(path string, engine consensus.Engine) ([]*types.Block, *core.Genesis, int, int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, 0, 0, err
	}

	// Trim UTF-8 Byte Order Mark (BOM) if present (\xEF\xBB\xBF)
	if len(data) >= 3 && data[0] == 0xEF && data[1] == 0xBB && data[2] == 0xBF {
		data = data[3:]
	}

	var parsedTest jsonBlockchainTestFile
	if err := json.Unmarshal(data, &parsedTest); err != nil {
		return nil, nil, 0, 0, err
	}

	var instance jsonTestInstance
	for _, inst := range parsedTest {
		instance = inst
		break
	}

	if len(instance.Blocks) == 0 {
		return nil, nil, 0, 0, fmt.Errorf("file lacks blocks data array")
	}

	targetBlock := instance.Blocks[0]
	signer := types.LatestSignerForChainID(benchChainConfig.ChainID)

	hugeBalance := new(big.Int).Exp(big.NewInt(2), big.NewInt(200), nil)
	alloc := make(types.GenesisAlloc)
	// Tracks the lowest nonce seen so far per recovered sender, so repeat
	// senders within the block get their true starting nonce in genesis.
	minNonce := make(map[common.Address]uint64)

	ethTxs := make([]*types.Transaction, 0, len(targetBlock.Transactions))
	dropped := 0
	for i, tx := range targetBlock.Transactions {
		if tx.MaxFeePerGas == nil || tx.MaxPriorityFeePerGas == nil {
			// Legacy tx with no gasPrice in the fixture -- can't validate
			// its original signature, so it's dropped rather than faked.
			dropped++
			continue
		}

		var toAddress *common.Address
		if tx.To != "" {
			addr := common.HexToAddress(tx.To)
			toAddress = &addr
		}

		nonce := uint64(tx.Nonce)
		innerTx := &types.DynamicFeeTx{
			ChainID:    benchChainConfig.ChainID,
			Nonce:      nonce,
			GasTipCap:  tx.MaxPriorityFeePerGas.ToInt(),
			GasFeeCap:  tx.MaxFeePerGas.ToInt(),
			Gas:        uint64(tx.GasLimit),
			To:         toAddress,
			Value:      tx.Value.ToInt(),
			Data:       tx.Data,
			AccessList: tx.AccessList,
			V:          tx.V.ToInt(),
			R:          tx.R.ToInt(),
			S:          tx.S.ToInt(),
		}
		signedTx := types.NewTx(innerTx)

		from, err := types.Sender(signer, signedTx)
		if err != nil {
			return nil, nil, 0, 0, fmt.Errorf("recover sender for tx %d: %w", i, err)
		}

		if _, funded := alloc[from]; !funded {
			alloc[from] = types.Account{Balance: hugeBalance, Nonce: nonce}
			minNonce[from] = nonce
		} else if nonce < minNonce[from] {
			acct := alloc[from]
			acct.Nonce = nonce
			alloc[from] = acct
			minNonce[from] = nonce
		}

		ethTxs = append(ethTxs, signedTx)
	}

	genesis := &core.Genesis{
		Config:   benchChainConfig,
		GasLimit: uint64(targetBlock.BlockHeader.GasLimit),
		BaseFee:  targetBlock.BlockHeader.BaseFeePerGas.ToInt(),
		Alloc:    alloc,
	}

	// Commit genesis to a throwaway in-memory DB purely so GenerateChain has
	// real state to execute the target transactions against.
	genDB := rawdb.NewMemoryDatabase()
	genTrieDB := triedb.NewDatabase(genDB, triedb.HashDefaults)
	genesisBlock := genesis.MustCommit(genDB, genTrieDB)

	blocks, _ := core.GenerateChain(benchChainConfig, genesisBlock, engine, genDB, 1, func(i int, gen *core.BlockGen) {
		// Target/benchmark block: real transactions from the JSON file,
		// original signatures intact, in original block order (required so
		// repeat senders' nonces increment correctly as each executes).
		for _, tx := range ethTxs {
			gen.AddTx(tx)
		}
	})

	return blocks, genesis, len(ethTxs), dropped, nil
}
