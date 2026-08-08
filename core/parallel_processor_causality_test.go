package core

import (
	"crypto/ecdsa"
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/params"
)

func signLegacy(t *testing.T, key *ecdsa.PrivateKey, nonce uint64, to common.Address) *types.Transaction {
	t.Helper()
	tx := types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		GasPrice: big.NewInt(1),
		Gas:      21000,
		To:       &to,
		Value:    big.NewInt(0),
	})
	signer := types.LatestSignerForChainID(params.TestChainConfig.ChainID)
	signed, err := types.SignTx(tx, signer, key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return signed
}

// Tx 0→A, tx 1→B from same sender (nonces 0,1), tx 2→A from other sender.
// Old packer could emit {0,1-sibling-wrong} as {0 with later same-sender} before {1}.
// With A/B pattern: seed 0 (to A), skip 1 (same from), add 2? 2 goes to A overlaps.
// Better explicit case matching the bug:
//   0: sender S0 → token A
//   1: sender S1 → token A  (conflicts with 0)
//   2: sender S0 → token B  (same sender as 0, next nonce; no overlap with {0}? wait 0 and 2 same from)
// Same from always overlaps. Need:
//   0: S0 → A
//   1: S1 → A
//   2: S0 → B  — 0 and 2 same S0, conflict with each other
// Building wave from 0: addresses {S0,A}. Scan 1: overlaps A. Scan 2: overlaps S0. Neither joins. OK.
//
// Bug case:
//   0: S1 → A
//   1: S0 → A   (conflicts with 0 via A)
//   2: S0 → B   (same S0 as 1; does not overlap {S1,A})
// Old: wave0={0,2}, wave1={1} → nonce of S0: 2 before 1.
// New: 2 blocked while 1 unassigned → wave0={0}, wave1={1}, wave2={2}.
func TestBuildTransactionStorageParallelGroupsCausalOrder(t *testing.T) {
	ParallelTxGroupingByStorageOverlap = true
	defer func() { ParallelTxGroupingByStorageOverlap = true }()

	k0, _ := crypto.GenerateKey()
	k1, _ := crypto.GenerateKey()
	tokenA := common.HexToAddress("0xA")
	tokenB := common.HexToAddress("0xB")

	txs := []*types.Transaction{
		signLegacy(t, k1, 0, tokenA), // 0: S1 → A
		signLegacy(t, k0, 0, tokenA), // 1: S0 → A
		signLegacy(t, k0, 1, tokenB), // 2: S0 → B
	}
	signer := types.LatestSignerForChainID(params.TestChainConfig.ChainID)
	groups, err := BuildTransactionStorageParallelGroups(txs, signer)
	if err != nil {
		t.Fatal(err)
	}

	// Find which wave contains each index and ensure wave(1) before wave(2).
	waveOf := map[int]int{}
	for wi, g := range groups {
		for _, idx := range g {
			waveOf[idx] = wi
		}
	}
	if waveOf[1] >= waveOf[2] {
		t.Fatalf("causality broken: wave(tx1)=%d wave(tx2)=%d groups=%v", waveOf[1], waveOf[2], groups)
	}
	// Tx 2 must not share a wave with tx 0 while tx 1 is pending — i.e. not {0,2}.
	for _, g := range groups {
		has0, has2 := false, false
		for _, idx := range g {
			if idx == 0 {
				has0 = true
			}
			if idx == 2 {
				has2 = true
			}
		}
		if has0 && has2 {
			t.Fatalf("tx0 and tx2 co-scheduled before causality fix: %v", groups)
		}
	}
}
