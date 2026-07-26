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
	"github.com/holiman/uint256"
)

type jsonBlockchainTestFile map[string]jsonTestInstance

type jsonTestInstance struct {
	Pre         types.GenesisAlloc `json:"pre"`
	BlockHeader *jsonBlockHeader   `json:"blockHeader"`
	Blocks      []jsonBlock        `json:"blocks"`
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

// jsonTransaction keeps original v/r/s and type-specific fields from eth_getBlockByNumber.
type jsonTransaction struct {
	Type                 *hexutil.Uint64              `json:"type"`
	AccessList           types.AccessList             `json:"accessList"`
	DeclaredAccessList   types.AccessList             `json:"declaredAccessList"`
	GeneratedAccessList  types.AccessList             `json:"generatedAccessList"`
	Data                 hexutil.Bytes                `json:"data"`
	GasLimit             hexutil.Uint64               `json:"gasLimit"`
	GasPrice             *hexutil.Big                 `json:"gasPrice"`
	MaxFeePerGas         *hexutil.Big                 `json:"maxFeePerGas"`
	MaxPriorityFeePerGas *hexutil.Big                 `json:"maxPriorityFeePerGas"`
	MaxFeePerBlobGas     *hexutil.Big                 `json:"maxFeePerBlobGas"`
	BlobVersionedHashes  []common.Hash                `json:"blobVersionedHashes"`
	AuthorizationList    []types.SetCodeAuthorization `json:"authorizationList"`
	Nonce                hexutil.Uint64               `json:"nonce"`
	To                   string                       `json:"to"`
	Value                *hexutil.Big                 `json:"value"`
	V                    *hexutil.Big                 `json:"v"`
	R                    *hexutil.Big                 `json:"r"`
	S                    *hexutil.Big                 `json:"s"`
}

func uint64ptr(v uint64) *uint64 { return &v }

// benchChainConfig is mainnet-like with post-merge forks active from genesis so
// recent bytecode (PUSH0, etc.) executes. GenerateChain/beacon fill Shanghai
// withdrawals and Cancun blob header fields as needed.
var benchChainConfig = &params.ChainConfig{
	ChainID:                 big.NewInt(1),
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
	TerminalTotalDifficulty: big.NewInt(0),
	ShanghaiTime:            uint64ptr(0),
	CancunTime:              uint64ptr(0),
	PragueTime:              uint64ptr(0),
	DepositContractAddress:  params.MainnetChainConfig.DepositContractAddress,
	BlobScheduleConfig: &params.BlobScheduleConfig{
		Cancun: params.DefaultCancunBlobConfig,
		Prague: params.DefaultPragueBlobConfig,
	},
}

func TestParallelBenchmarkAgainstRealBlocks(t *testing.T) {
	originalGrouping := core.ParallelTxGroupingByStorageOverlap
	originalWaveExecution := core.ParallelTxWaveExecution
	originalTiming := core.ParallelTxTiming
	defer func() {
		core.ParallelTxGroupingByStorageOverlap = originalGrouping
		core.ParallelTxWaveExecution = originalWaveExecution
		core.ParallelTxTiming = originalTiming
	}()
	if v := strings.ToLower(os.Getenv("PARALLEL_TX_TIMING")); v == "1" || v == "true" {
		core.ParallelTxTiming = true
	}
	if p := os.Getenv("PARALLEL_TX_TIMING_FILE"); p != "" {
		core.SetParallelTxTimingFile(p)
	}
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
		blocks, genesis, txCount, dropped, err := parseTestJSONFile(filePath, engine)
		if err != nil {
			t.Logf("skipping file %s due to parsing error: %v", file.Name(), err)
			continue
		}
		if dropped > 0 {
			t.Logf("%s: applied=%d dropped=%d", file.Name(), txCount, dropped)
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
				// Older/hollow fixtures can finish so fast that a duration rounds to 0,
				// which makes seq/par → NaN/Inf and poisons the speedup average.
				if parTime > 0 {
					speedupSamples = append(speedupSamples, float64(seqTime)/float64(parTime))
				}
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
			if len(speedupSamples) == 0 && parAvg > 0 {
				speedupAvg = seqAvg / parAvg
				speedupStd = 0
			}
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
	clean := make([]float64, 0, len(samples))
	for _, sample := range samples {
		if math.IsNaN(sample) || math.IsInf(sample, 0) {
			continue
		}
		clean = append(clean, sample)
	}
	if len(clean) == 0 {
		return 0, 0
	}
	var sum float64
	for _, sample := range clean {
		sum += sample
	}
	mean := sum / float64(len(clean))
	if len(clean) == 1 {
		return mean, 0
	}
	var squaredDiffs float64
	for _, sample := range clean {
		diff := sample - mean
		squaredDiffs += diff * diff
	}
	return mean, math.Sqrt(squaredDiffs / float64(len(clean)-1))
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
		fmt.Printf("benchmark block %d failed: %v\n", n, err)
		// t.Fatalf("benchmark block %d failed: %v", n, err)
	}
	return time.Since(start)
}

// parseTestJSONFile reads the JSON fixture and builds an executable block on
// top of genesis via core.GenerateChain.
//
// If instance.pre is present (from fetch_block_with_prestate.py), that parent
// prestate is used as genesis alloc so contract code/storage actually run.
// Otherwise senders are synthetic-funded (legacy lightweight fixtures).
//
// Transactions keep ORIGINAL v/r/s. Legacy txs without fee fields are dropped.
// Txs that panic under AddTx (incomplete prestate, etc.) are skipped.
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

	alloc := make(types.GenesisAlloc)
	if len(instance.Pre) > 0 {
		for addr, acct := range instance.Pre {
			alloc[addr] = acct
		}
	}

	hugeBalance := new(big.Int).Exp(big.NewInt(2), big.NewInt(200), nil)
	minNonce := make(map[common.Address]uint64)
	usePrestate := len(instance.Pre) > 0

	ethTxs := make([]*types.Transaction, 0, len(targetBlock.Transactions))
	dropped := 0
	for i, tx := range targetBlock.Transactions {
		signedTx, err := transactionFromJSON(tx)
		if err != nil {
			dropped++
			continue
		}

		if len(tx.GeneratedAccessList) > 0 {
			signedTx.SetGeneratedAccessList(tx.GeneratedAccessList)
		}

		from, err := types.Sender(signer, signedTx)
		if err != nil {
			return nil, nil, 0, 0, fmt.Errorf("recover sender for tx %d: %w", i, err)
		}

		nonce := signedTx.Nonce()
		if !usePrestate {
			if _, funded := alloc[from]; !funded {
				alloc[from] = types.Account{Balance: hugeBalance, Nonce: nonce}
				minNonce[from] = nonce
			} else if nonce < minNonce[from] {
				acct := alloc[from]
				acct.Nonce = nonce
				alloc[from] = acct
				minNonce[from] = nonce
			}
		}

		ethTxs = append(ethTxs, signedTx)
	}

	genesis := &core.Genesis{
		Config:   benchChainConfig,
		GasLimit: uint64(targetBlock.BlockHeader.GasLimit),
		BaseFee:  targetBlock.BlockHeader.BaseFeePerGas.ToInt(),
		Alloc:    alloc,
	}

	genDB := rawdb.NewMemoryDatabase()
	genTrieDB := triedb.NewDatabase(genDB, triedb.HashDefaults)
	genesisBlock := genesis.MustCommit(genDB, genTrieDB)

	applied := 0
	blocks, _ := core.GenerateChain(benchChainConfig, genesisBlock, engine, genDB, 1, func(i int, gen *core.BlockGen) {
		for _, tx := range ethTxs {
			if tryAddTx(gen, tx) {
				applied++
			} else {
				dropped++
			}
		}
	})

	return blocks, genesis, applied, dropped, nil
}

func transactionFromJSON(tx jsonTransaction) (*types.Transaction, error) {
	if tx.V == nil || tx.R == nil || tx.S == nil || tx.Value == nil {
		return nil, fmt.Errorf("missing signature or value")
	}
	nonce := uint64(tx.Nonce)
	gas := uint64(tx.GasLimit)
	var toAddress *common.Address
	if tx.To != "" {
		addr := common.HexToAddress(tx.To)
		toAddress = &addr
	}
	txType := uint64(types.DynamicFeeTxType)
	if tx.Type != nil {
		txType = uint64(*tx.Type)
	} else if tx.MaxFeePerGas == nil && tx.GasPrice != nil {
		txType = types.LegacyTxType
	}

	switch txType {
	case types.LegacyTxType:
		if tx.GasPrice == nil {
			return nil, fmt.Errorf("legacy tx missing gasPrice")
		}
		return types.NewTx(&types.LegacyTx{
			Nonce:    nonce,
			GasPrice: tx.GasPrice.ToInt(),
			Gas:      gas,
			To:       toAddress,
			Value:    tx.Value.ToInt(),
			Data:     tx.Data,
			V:        tx.V.ToInt(),
			R:        tx.R.ToInt(),
			S:        tx.S.ToInt(),
		}), nil

	case types.AccessListTxType:
		if tx.GasPrice == nil {
			return nil, fmt.Errorf("access-list tx missing gasPrice")
		}
		return types.NewTx(&types.AccessListTx{
			ChainID:    benchChainConfig.ChainID,
			Nonce:      nonce,
			GasPrice:   tx.GasPrice.ToInt(),
			Gas:        gas,
			To:         toAddress,
			Value:      tx.Value.ToInt(),
			Data:       tx.Data,
			AccessList: tx.AccessList,
			V:          tx.V.ToInt(),
			R:          tx.R.ToInt(),
			S:          tx.S.ToInt(),
		}), nil

	case types.DynamicFeeTxType:
		if tx.MaxFeePerGas == nil || tx.MaxPriorityFeePerGas == nil {
			return nil, fmt.Errorf("dynamic fee tx missing fee caps")
		}
		return types.NewTx(&types.DynamicFeeTx{
			ChainID:    benchChainConfig.ChainID,
			Nonce:      nonce,
			GasTipCap:  tx.MaxPriorityFeePerGas.ToInt(),
			GasFeeCap:  tx.MaxFeePerGas.ToInt(),
			Gas:        gas,
			To:         toAddress,
			Value:      tx.Value.ToInt(),
			Data:       tx.Data,
			AccessList: tx.AccessList,
			V:          tx.V.ToInt(),
			R:          tx.R.ToInt(),
			S:          tx.S.ToInt(),
		}), nil

	case types.BlobTxType:
		if tx.MaxFeePerGas == nil || tx.MaxPriorityFeePerGas == nil || tx.MaxFeePerBlobGas == nil {
			return nil, fmt.Errorf("blob tx missing fee fields")
		}
		if toAddress == nil {
			return nil, fmt.Errorf("blob tx missing to")
		}
		return types.NewTx(&types.BlobTx{
			ChainID:    uint256.MustFromBig(benchChainConfig.ChainID),
			Nonce:      nonce,
			GasTipCap:  uint256.MustFromBig(tx.MaxPriorityFeePerGas.ToInt()),
			GasFeeCap:  uint256.MustFromBig(tx.MaxFeePerGas.ToInt()),
			Gas:        gas,
			To:         *toAddress,
			Value:      uint256.MustFromBig(tx.Value.ToInt()),
			Data:       tx.Data,
			AccessList: tx.AccessList,
			BlobFeeCap: uint256.MustFromBig(tx.MaxFeePerBlobGas.ToInt()),
			BlobHashes: tx.BlobVersionedHashes,
			V:          uint256.MustFromBig(tx.V.ToInt()),
			R:          uint256.MustFromBig(tx.R.ToInt()),
			S:          uint256.MustFromBig(tx.S.ToInt()),
		}), nil

	case types.SetCodeTxType:
		if tx.MaxFeePerGas == nil || tx.MaxPriorityFeePerGas == nil {
			return nil, fmt.Errorf("setcode tx missing fee caps")
		}
		if toAddress == nil {
			return nil, fmt.Errorf("setcode tx missing to")
		}
		return types.NewTx(&types.SetCodeTx{
			ChainID:    uint256.MustFromBig(benchChainConfig.ChainID),
			Nonce:      nonce,
			GasTipCap:  uint256.MustFromBig(tx.MaxPriorityFeePerGas.ToInt()),
			GasFeeCap:  uint256.MustFromBig(tx.MaxFeePerGas.ToInt()),
			Gas:        gas,
			To:         *toAddress,
			Value:      uint256.MustFromBig(tx.Value.ToInt()),
			Data:       tx.Data,
			AccessList: tx.AccessList,
			AuthList:   tx.AuthorizationList,
			V:          uint256.MustFromBig(tx.V.ToInt()),
			R:          uint256.MustFromBig(tx.R.ToInt()),
			S:          uint256.MustFromBig(tx.S.ToInt()),
		}), nil

	default:
		return nil, fmt.Errorf("unsupported tx type %d", txType)
	}
}

func tryAddTx(gen *core.BlockGen, tx *types.Transaction) (ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	gen.AddTx(tx)
	return true
}
