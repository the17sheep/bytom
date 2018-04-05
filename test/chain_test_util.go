package test

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	dbm "github.com/tendermint/tmlibs/db"

	"github.com/bytom/blockchain/txbuilder"
	"github.com/bytom/consensus"
	"github.com/bytom/database/leveldb"
	"github.com/bytom/database/storage"
	"github.com/bytom/protocol"
	"github.com/bytom/protocol/bc"
	"github.com/bytom/protocol/bc/types"
	"github.com/bytom/protocol/vm"
)

const utxoPrefix = "UT:"

type ChainTestContext struct {
	Chain *protocol.Chain
	DB    dbm.DB
}

func (ctx *ChainTestContext) append(blkNum uint64) error {
	for i := uint64(0); i < blkNum; i++ {
		prevBlock := ctx.Chain.BestBlock()
		timestamp := prevBlock.Timestamp + defaultDuration
		prevBlockHash := prevBlock.Hash()
		block, err := DefaultEmptyBlock(prevBlock.Height+1, timestamp, prevBlockHash, prevBlock.Bits)
		if err != nil {
			return err
		}
		if err := SolveAndUpdate(ctx.Chain, block); err != nil {
			return nil
		}
	}
	return nil
}

func (ctx *ChainTestContext) validateStatus(block *types.Block) error {
	// validate in mainchain
	if !ctx.Chain.InMainChain(block.Height, block.Hash()) {
		return fmt.Errorf("block %d is not in mainchain", block.Height)
	}

	// validate chain status and saved block
	bestBlock := ctx.Chain.BestBlock()
	chainBlock, err := ctx.Chain.GetBlockByHeight(block.Height)
	if err != nil {
		return err
	}

	blockHash := block.Hash()
	if bestBlock.Hash() != blockHash || chainBlock.Hash() != blockHash {
		return fmt.Errorf("chain status error")
	}

	// validate tx status
	txStatus, err := ctx.Chain.GetTransactionStatus(&blockHash)
	if err != nil {
		return err
	}

	txStatusMerkleRoot, err := bc.TxStatusMerkleRoot(txStatus.VerifyStatus)
	if err != nil {
		return err
	}

	if txStatusMerkleRoot != block.TransactionStatusHash {
		return fmt.Errorf("tx status error")
	}
	return nil
}

func (ctx *ChainTestContext) validateExecution(block *types.Block) error {
	for _, tx := range block.Transactions {
		for _, spentOutputID := range tx.SpentOutputIDs {
			utxoEntry, _ := leveldb.GetUtxo(ctx.DB, &spentOutputID)
			if utxoEntry == nil {
				continue
			}
			if !utxoEntry.IsCoinBase {
				return fmt.Errorf("found non-coinbase spent utxo entry")
			}
			if !utxoEntry.Spent {
				return fmt.Errorf("utxo entry status should be spent")
			}
		}

		for _, outputID := range tx.ResultIds {
			utxoEntry, _ := leveldb.GetUtxo(ctx.DB, outputID)
			if utxoEntry == nil && isSpent(outputID, block) {
				continue
			}
			if utxoEntry.BlockHeight != block.Height {
				return fmt.Errorf("block height error, expected: %d, have: %d", block.Height, utxoEntry.BlockHeight)
			}
			if utxoEntry.Spent {
				return fmt.Errorf("utxo entry status should not be spent")
			}
		}
	}
	return nil
}

func (ctx *ChainTestContext) getUtxoEntries() map[string]*storage.UtxoEntry {
	utxoEntries := make(map[string]*storage.UtxoEntry)
	iter := ctx.DB.IteratorPrefix([]byte(utxoPrefix))
	defer iter.Release()

	for iter.Next() {
		utxoEntry := storage.UtxoEntry{}
		if err := json.Unmarshal(iter.Value(), &utxoEntry); err != nil {
			return nil
		}
		key := string(iter.Key())
		utxoEntries[key] = &utxoEntry
	}
	return utxoEntries
}

func (ctx *ChainTestContext) validateRollback(utxoEntries map[string]*storage.UtxoEntry) error {
	newUtxoEntries := ctx.getUtxoEntries()
	beforeRollbackLen := len(utxoEntries)
	nowLen := len(newUtxoEntries)
	if nowLen != beforeRollbackLen {
		return fmt.Errorf("now we have %d utxo entries, before rollback we have %d", nowLen, beforeRollbackLen)
	}

	for key, entry := range utxoEntries {
		if *entry != *newUtxoEntries[key] {
			return fmt.Errorf("can't find utxo entry after rollback")
		}
	}
	return nil
}

type ChainTestConfig struct {
	RollbackTo uint64     `json:"rollback_to"`
	Blocks     []*ctBlock `json:"blocks"`
}

type ctBlock struct {
	Transactions []*ctTransaction `json:"transactions"`
	Append       uint64           `json:"append"`
}

func (b *ctBlock) createBlock(ctx *ChainTestContext) (*types.Block, error) {
	txs := make([]*types.Tx, 0, len(b.Transactions))
	for _, t := range b.Transactions {
		tx, err := t.createTransaction(ctx, txs)
		if err != nil {
			return nil, err
		}
		txs = append(txs, tx)
	}
	return NewBlock(ctx.Chain, txs, []byte{byte(vm.OP_TRUE)})
}

type ctTransaction struct {
	Inputs  []*ctInput `json:"inputs"`
	Outputs []uint64   `json:"outputs"`
}

type ctInput struct {
	Height      uint64 `json:"height"`
	TxIndex     uint64 `json:"tx_index"`
	OutputIndex uint64 `json:"output_index"`
}

func (input *ctInput) createTxInput(ctx *ChainTestContext) (*types.TxInput, error) {
	block, err := ctx.Chain.GetBlockByHeight(input.Height)
	if err != nil {
		return nil, err
	}

	spendInput, err := CreateSpendInput(block.Transactions[input.TxIndex], input.OutputIndex)
	if err != nil {
		return nil, err
	}

	return &types.TxInput{
		AssetVersion: assetVersion,
		TypedInput:   spendInput,
	}, nil
}

// create tx input spent previous tx output in the same block
func (input *ctInput) createDependencyTxInput(txs []*types.Tx) (*types.TxInput, error) {
	// sub 1 because of coinbase tx is not included in txs
	spendInput, err := CreateSpendInput(txs[input.TxIndex-1], input.OutputIndex)
	if err != nil {
		return nil, err
	}

	return &types.TxInput{
		AssetVersion: assetVersion,
		TypedInput:   spendInput,
	}, nil
}

func (t *ctTransaction) createTransaction(ctx *ChainTestContext, txs []*types.Tx) (*types.Tx, error) {
	builder := txbuilder.NewBuilder(time.Now())
	sigInst := &txbuilder.SigningInstruction{}
	currentHeight := ctx.Chain.Height()
	for _, input := range t.Inputs {
		var txInput *types.TxInput
		var err error
		if input.Height == currentHeight+1 {
			txInput, err = input.createDependencyTxInput(txs)
		} else {
			txInput, err = input.createTxInput(ctx)
		}
		if err != nil {
			return nil, err
		}
		builder.AddInput(txInput, sigInst)
	}

	for _, amount := range t.Outputs {
		output := types.NewTxOutput(*consensus.BTMAssetID, amount, []byte{byte(vm.OP_TRUE)})
		builder.AddOutput(output)
	}

	tpl, _, err := builder.Build()
	return tpl.Transaction, err
}

func (cfg *ChainTestConfig) Run() error {
	db := dbm.NewDB("chain_test_db", "leveldb", "chain_test_db")
	defer os.RemoveAll("chain_test_db")
	chain, _ := MockChain(db)
	ctx := &ChainTestContext{
		Chain: chain,
		DB:    db,
	}

	var utxoEntries map[string]*storage.UtxoEntry
	var rollbackBlock *types.Block
	for _, blk := range cfg.Blocks {
		block, err := blk.createBlock(ctx)
		if err != nil {
			return err
		}
		if err := SolveAndUpdate(ctx.Chain, block); err != nil {
			return err
		}
		if err := ctx.validateStatus(block); err != nil {
			return err
		}
		if err := ctx.validateExecution(block); err != nil {
			return err
		}
		if block.Height <= cfg.RollbackTo && cfg.RollbackTo <= block.Height+blk.Append {
			utxoEntries = ctx.getUtxoEntries()
			rollbackBlock = block
		}
		if err := ctx.append(blk.Append); err != nil {
			return err
		}
	}

	if rollbackBlock == nil {
		return nil
	}

	// rollback and validate
	if err := ctx.Chain.ReorganizeChain(rollbackBlock); err != nil {
		return err
	}
	if err := ctx.validateRollback(utxoEntries); err != nil {
		return err
	}
	if err := ctx.validateStatus(rollbackBlock); err != nil {
		return err
	}
	return nil
}

// if the output(hash) was spent in block
func isSpent(hash *bc.Hash, block *types.Block) bool {
	for _, tx := range block.Transactions {
		for _, spendOutputID := range tx.SpentOutputIDs {
			if spendOutputID == *hash {
				return true
			}
		}
	}
	return false
}
