// Copyright 2018 The go-ethereum Authors
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

package miner

import (
	"math"
	"math/big"
	"math/rand"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"

	"github.com/scroll-tech/go-ethereum/accounts"
	"github.com/scroll-tech/go-ethereum/common"
	"github.com/scroll-tech/go-ethereum/consensus"
	"github.com/scroll-tech/go-ethereum/consensus/clique"
	"github.com/scroll-tech/go-ethereum/consensus/ethash"
	"github.com/scroll-tech/go-ethereum/core"
	"github.com/scroll-tech/go-ethereum/core/rawdb"
	"github.com/scroll-tech/go-ethereum/core/types"
	"github.com/scroll-tech/go-ethereum/core/vm"
	"github.com/scroll-tech/go-ethereum/crypto"
	"github.com/scroll-tech/go-ethereum/ethdb"
	"github.com/scroll-tech/go-ethereum/event"
	"github.com/scroll-tech/go-ethereum/params"
	"github.com/scroll-tech/go-ethereum/rollup/circuitcapacitychecker"
	"github.com/scroll-tech/go-ethereum/rollup/sync_service"
)

const (
	// testCode is the testing contract binary code which will initialises some
	// variables in constructor
	testCode = "0x60806040527fffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff0060005534801561003457600080fd5b5060fc806100436000396000f3fe6080604052348015600f57600080fd5b506004361060325760003560e01c80630c4dae8814603757806398a213cf146053575b600080fd5b603d607e565b6040518082815260200191505060405180910390f35b607c60048036036020811015606757600080fd5b81019080803590602001909291905050506084565b005b60005481565b806000819055507fe9e44f9f7da8c559de847a3232b57364adc0354f15a2cd8dc636d54396f9587a6000546040518082815260200191505060405180910390a15056fea265627a7a723058208ae31d9424f2d0bc2a3da1a5dd659db2d71ec322a17db8f87e19e209e3a1ff4a64736f6c634300050a0032"

	// testGas is the gas required for contract deployment.
	testGas = 144109
)

var (
	// Test chain configurations
	testTxPoolConfig  core.TxPoolConfig
	ethashChainConfig *params.ChainConfig
	cliqueChainConfig *params.ChainConfig

	// Test accounts
	testBankKey, _  = crypto.GenerateKey()
	testBankAddress = crypto.PubkeyToAddress(testBankKey.PublicKey)
	testBankFunds   = big.NewInt(1000000000000000000)

	testUserKey, _  = crypto.GenerateKey()
	testUserAddress = crypto.PubkeyToAddress(testUserKey.PublicKey)

	// Test transactions
	pendingTxs []*types.Transaction
	newTxs     []*types.Transaction

	testConfig = &Config{
		Recommit:       time.Second,
		GasCeil:        params.GenesisGasLimit,
		MaxAccountsNum: math.MaxInt,
	}
)

func init() {
	testTxPoolConfig = core.DefaultTxPoolConfig
	testTxPoolConfig.Journal = ""
	ethashChainConfig = new(params.ChainConfig)
	*ethashChainConfig = *params.TestChainConfig
	cliqueChainConfig = new(params.ChainConfig)
	*cliqueChainConfig = *params.TestChainConfig
	cliqueChainConfig.Clique = &params.CliqueConfig{
		Period: 10,
		Epoch:  30000,
	}

	signer := types.LatestSigner(params.TestChainConfig)
	tx1 := types.MustSignNewTx(testBankKey, signer, &types.AccessListTx{
		ChainID:  params.TestChainConfig.ChainID,
		Nonce:    0,
		To:       &testUserAddress,
		Value:    big.NewInt(1000),
		Gas:      params.TxGas,
		GasPrice: big.NewInt(params.InitialBaseFee),
	})
	pendingTxs = append(pendingTxs, tx1)

	tx2 := types.MustSignNewTx(testBankKey, signer, &types.LegacyTx{
		Nonce:    1,
		To:       &testUserAddress,
		Value:    big.NewInt(1000),
		Gas:      params.TxGas,
		GasPrice: big.NewInt(params.InitialBaseFee),
	})
	newTxs = append(newTxs, tx2)

	rand.Seed(time.Now().UnixNano())
}

// testWorkerBackend implements worker.Backend interfaces and wraps all information needed during the testing.
type testWorkerBackend struct {
	db         ethdb.Database
	txPool     *core.TxPool
	chain      *core.BlockChain
	testTxFeed event.Feed
	genesis    *core.Genesis
	uncleBlock *types.Block
}

func newTestWorkerBackend(t *testing.T, chainConfig *params.ChainConfig, engine consensus.Engine, db ethdb.Database, n int) *testWorkerBackend {
	var gspec = core.Genesis{
		Config: chainConfig,
		Alloc:  core.GenesisAlloc{testBankAddress: {Balance: testBankFunds}},
	}

	switch e := engine.(type) {
	case *clique.Clique:
		gspec.ExtraData = make([]byte, 32+common.AddressLength+crypto.SignatureLength)
		gspec.Timestamp = uint64(time.Now().Unix())
		copy(gspec.ExtraData[32:32+common.AddressLength], testBankAddress.Bytes())
		e.Authorize(testBankAddress, func(account accounts.Account, s string, data []byte) ([]byte, error) {
			return crypto.Sign(crypto.Keccak256(data), testBankKey)
		})
	case *ethash.Ethash:
	default:
		t.Fatalf("unexpected consensus engine type: %T", engine)
	}
	genesis := gspec.MustCommit(db)

	chain, _ := core.NewBlockChain(db, &core.CacheConfig{TrieDirtyDisabled: true}, gspec.Config, engine, vm.Config{
		Debug:  true,
		Tracer: vm.NewStructLogger(&vm.LogConfig{EnableMemory: true})}, nil, nil)
	txpool := core.NewTxPool(testTxPoolConfig, chainConfig, chain)

	// Generate a small n-block chain and an uncle block for it
	if n > 0 {
		blocks, _ := core.GenerateChain(chainConfig, genesis, engine, db, n, func(i int, gen *core.BlockGen) {
			gen.SetCoinbase(testBankAddress)
		})
		if _, err := chain.InsertChain(blocks); err != nil {
			t.Fatalf("failed to insert origin chain: %v", err)
		}
	}
	parent := genesis
	if n > 0 {
		parent = chain.GetBlockByHash(chain.CurrentBlock().ParentHash())
	}
	blocks, _ := core.GenerateChain(chainConfig, parent, engine, db, 1, func(i int, gen *core.BlockGen) {
		gen.SetCoinbase(testUserAddress)
	})

	return &testWorkerBackend{
		db:         db,
		chain:      chain,
		txPool:     txpool,
		genesis:    &gspec,
		uncleBlock: blocks[0],
	}
}

func (b *testWorkerBackend) BlockChain() *core.BlockChain           { return b.chain }
func (b *testWorkerBackend) TxPool() *core.TxPool                   { return b.txPool }
func (b *testWorkerBackend) ChainDb() ethdb.Database                { return b.db }
func (b *testWorkerBackend) SyncService() *sync_service.SyncService { return nil }

func (b *testWorkerBackend) newRandomUncle() *types.Block {
	var parent *types.Block
	cur := b.chain.CurrentBlock()
	if cur.NumberU64() == 0 {
		parent = b.chain.Genesis()
	} else {
		parent = b.chain.GetBlockByHash(b.chain.CurrentBlock().ParentHash())
	}
	blocks, _ := core.GenerateChain(b.chain.Config(), parent, b.chain.Engine(), b.db, 1, func(i int, gen *core.BlockGen) {
		var addr = make([]byte, common.AddressLength)
		rand.Read(addr)
		gen.SetCoinbase(common.BytesToAddress(addr))
	})
	return blocks[0]
}

func (b *testWorkerBackend) newRandomTx(creation bool) *types.Transaction {
	var tx *types.Transaction
	gasPrice := big.NewInt(10 * params.InitialBaseFee)
	if creation {
		tx, _ = types.SignTx(types.NewContractCreation(b.txPool.Nonce(testBankAddress), big.NewInt(0), testGas, gasPrice, common.FromHex(testCode)), types.HomesteadSigner{}, testBankKey)
	} else {
		tx, _ = types.SignTx(types.NewTransaction(b.txPool.Nonce(testBankAddress), testUserAddress, big.NewInt(1000), params.TxGas, gasPrice, nil), types.HomesteadSigner{}, testBankKey)
	}
	return tx
}

func newTestWorker(t *testing.T, chainConfig *params.ChainConfig, engine consensus.Engine, db ethdb.Database, blocks int) (*worker, *testWorkerBackend) {
	backend := newTestWorkerBackend(t, chainConfig, engine, db, blocks)
	backend.txPool.AddLocals(pendingTxs)
	w := newWorker(testConfig, chainConfig, engine, backend, new(event.TypeMux), nil, false)
	w.setEtherbase(testBankAddress)
	return w, backend
}

func TestGenerateBlockAndImportClique(t *testing.T) {
	testGenerateBlockAndImport(t, true)
}

func testGenerateBlockAndImport(t *testing.T, isClique bool) {
	var (
		engine      consensus.Engine
		chainConfig *params.ChainConfig
		db          = rawdb.NewMemoryDatabase()
	)
	if isClique {
		chainConfig = params.AllCliqueProtocolChanges
		chainConfig.Clique = &params.CliqueConfig{Period: 1, Epoch: 30000}
		engine = clique.New(chainConfig.Clique, db)
	} else {
		chainConfig = params.AllEthashProtocolChanges
		engine = ethash.NewFaker()
	}
	chainConfig.Scroll.FeeVaultAddress = &common.Address{}

	chainConfig.LondonBlock = big.NewInt(0)
	w, b := newTestWorker(t, chainConfig, engine, db, 0)
	defer w.close()

	// This test chain imports the mined blocks.
	db2 := rawdb.NewMemoryDatabase()
	b.genesis.MustCommit(db2)
	chain, _ := core.NewBlockChain(db2, nil, b.chain.Config(), engine, vm.Config{
		Debug:  true,
		Tracer: vm.NewStructLogger(&vm.LogConfig{EnableMemory: true, EnableReturnData: true})}, nil, nil)
	defer chain.Stop()

	// Wait for mined blocks.
	sub := w.mux.Subscribe(core.NewMinedBlockEvent{})
	defer sub.Unsubscribe()

	// Start mining!
	w.start()

	for i := 0; i < 5; i++ {
		b.txPool.AddLocal(b.newRandomTx(true))
		b.txPool.AddLocal(b.newRandomTx(false))

		select {
		case ev := <-sub.Chan():
			block := ev.Data.(core.NewMinedBlockEvent).Block
			if _, err := chain.InsertChain([]*types.Block{block}); err != nil {
				t.Fatalf("failed to insert new mined block %d: %v", block.NumberU64(), err)
			}
		case <-time.After(3 * time.Second): // Worker needs 1s to include new changes.
			t.Fatalf("timeout")
		}
	}
}

func TestGenerateBlockWithL1MsgClique(t *testing.T) {
	testGenerateBlockWithL1Msg(t, true)
}

func testGenerateBlockWithL1Msg(t *testing.T, isClique bool) {
	assert := assert.New(t)
	var (
		engine      consensus.Engine
		chainConfig *params.ChainConfig
		db          = rawdb.NewMemoryDatabase()
	)
	msgs := []types.L1MessageTx{
		{QueueIndex: 0, Gas: 21016, To: &common.Address{3}, Data: []byte{0x01}, Sender: common.Address{4}},
		{QueueIndex: 1, Gas: 21016, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{2}}}
	rawdb.WriteL1Messages(db, msgs)

	if isClique {
		chainConfig = params.AllCliqueProtocolChanges
		chainConfig.Clique = &params.CliqueConfig{Period: 1, Epoch: 30000}
		engine = clique.New(chainConfig.Clique, db)
	} else {
		chainConfig = params.AllEthashProtocolChanges
		engine = ethash.NewFaker()
	}
	chainConfig.Scroll.L1Config = &params.L1Config{
		NumL1MessagesPerBlock: 1,
	}
	chainConfig.Scroll.FeeVaultAddress = &common.Address{}

	chainConfig.LondonBlock = big.NewInt(0)
	w, b := newTestWorker(t, chainConfig, engine, db, 0)
	defer w.close()

	// This test chain imports the mined blocks.
	b.genesis.MustCommit(db)
	chain, _ := core.NewBlockChain(db, nil, b.chain.Config(), engine, vm.Config{
		Debug:  true,
		Tracer: vm.NewStructLogger(&vm.LogConfig{EnableMemory: true, EnableReturnData: true})}, nil, nil)
	defer chain.Stop()

	// Wait for mined blocks.
	sub := w.mux.Subscribe(core.NewMinedBlockEvent{})
	defer sub.Unsubscribe()

	// Start mining!
	w.start()

	for i := 0; i < 2; i++ {

		select {
		case ev := <-sub.Chan():
			block := ev.Data.(core.NewMinedBlockEvent).Block
			if _, err := chain.InsertChain([]*types.Block{block}); err != nil {
				t.Fatalf("failed to insert new mined block %d: %v", block.NumberU64(), err)
			}
			assert.Equal(1, len(block.Transactions()))

			queueIndex := rawdb.ReadFirstQueueIndexNotInL2Block(db, block.Hash())
			assert.NotNil(queueIndex)
			assert.Equal(uint64(i+1), *queueIndex)
		case <-time.After(3 * time.Second):
			t.Fatalf("timeout")
		}
	}
}

func TestAcceptableTxlimit(t *testing.T) {
	assert := assert.New(t)
	var (
		engine      consensus.Engine
		chainConfig *params.ChainConfig
		db          = rawdb.NewMemoryDatabase()
	)
	chainConfig = params.AllCliqueProtocolChanges
	chainConfig.Clique = &params.CliqueConfig{Period: 1, Epoch: 30000}
	chainConfig.Scroll.FeeVaultAddress = &common.Address{}
	engine = clique.New(chainConfig.Clique, db)

	// Set maxTxPerBlock = 4, which >= non-l1msg + non-skipped l1msg txs
	maxTxPerBlock := 4
	chainConfig.Scroll.MaxTxPerBlock = &maxTxPerBlock
	chainConfig.Scroll.L1Config = &params.L1Config{
		NumL1MessagesPerBlock: 3,
	}

	// Insert 3 l1msgs, with one be skipped.
	l1msgs := []types.L1MessageTx{
		{QueueIndex: 0, Gas: 10000000, To: &common.Address{3}, Data: []byte{0x01}, Sender: common.Address{4}}, // over gas limit
		{QueueIndex: 1, Gas: 21016, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{4}},
		{QueueIndex: 2, Gas: 21016, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{2}}}
	rawdb.WriteL1Messages(db, l1msgs)

	chainConfig.LondonBlock = big.NewInt(0)
	w, b := newTestWorker(t, chainConfig, engine, db, 0)
	defer w.close()

	// This test chain imports the mined blocks.
	b.genesis.MustCommit(db)
	chain, _ := core.NewBlockChain(db, nil, b.chain.Config(), engine, vm.Config{
		Debug:  true,
		Tracer: vm.NewStructLogger(&vm.LogConfig{EnableMemory: true, EnableReturnData: true})}, nil, nil)
	defer chain.Stop()

	// Wait for mined blocks.
	sub := w.mux.Subscribe(core.NewMinedBlockEvent{})
	defer sub.Unsubscribe()

	// Insert 2 non-l1msg txs
	b.txPool.AddLocal(b.newRandomTx(true))
	b.txPool.AddLocal(b.newRandomTx(false))

	// Start mining!
	w.start()

	select {
	case ev := <-sub.Chan():
		block := ev.Data.(core.NewMinedBlockEvent).Block
		if _, err := chain.InsertChain([]*types.Block{block}); err != nil {
			t.Fatalf("failed to insert new mined block %d: %v", block.NumberU64(), err)
		}
		assert.Equal(4, len(block.Transactions()))
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout")
	}
}

func TestUnacceptableTxlimit(t *testing.T) {
	assert := assert.New(t)
	var (
		engine      consensus.Engine
		chainConfig *params.ChainConfig
		db          = rawdb.NewMemoryDatabase()
	)
	chainConfig = params.AllCliqueProtocolChanges
	chainConfig.Clique = &params.CliqueConfig{Period: 1, Epoch: 30000}
	chainConfig.Scroll.FeeVaultAddress = &common.Address{}
	engine = clique.New(chainConfig.Clique, db)

	// Set maxTxPerBlock = 3, which < non-l1msg + l1msg txs
	maxTxPerBlock := 3
	chainConfig.Scroll.MaxTxPerBlock = &maxTxPerBlock
	chainConfig.Scroll.L1Config = &params.L1Config{
		NumL1MessagesPerBlock: 2,
	}

	// Insert 2 l1msgs
	l1msgs := []types.L1MessageTx{
		{QueueIndex: 0, Gas: 21016, To: &common.Address{3}, Data: []byte{0x01}, Sender: common.Address{4}},
		{QueueIndex: 1, Gas: 21016, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{2}}}
	rawdb.WriteL1Messages(db, l1msgs)

	chainConfig.LondonBlock = big.NewInt(0)
	w, b := newTestWorker(t, chainConfig, engine, db, 0)
	defer w.close()

	// This test chain imports the mined blocks.
	b.genesis.MustCommit(db)
	chain, _ := core.NewBlockChain(db, nil, b.chain.Config(), engine, vm.Config{
		Debug:  true,
		Tracer: vm.NewStructLogger(&vm.LogConfig{EnableMemory: true, EnableReturnData: true})}, nil, nil)
	defer chain.Stop()

	// Wait for mined blocks.
	sub := w.mux.Subscribe(core.NewMinedBlockEvent{})
	defer sub.Unsubscribe()

	// Insert 2 non-l1msg txs
	b.txPool.AddLocal(b.newRandomTx(true))
	b.txPool.AddLocal(b.newRandomTx(false))

	// Start mining!
	w.start()

	select {
	case ev := <-sub.Chan():
		block := ev.Data.(core.NewMinedBlockEvent).Block
		if _, err := chain.InsertChain([]*types.Block{block}); err != nil {
			t.Fatalf("failed to insert new mined block %d: %v", block.NumberU64(), err)
		}
		assert.Equal(3, len(block.Transactions()))
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout")
	}
}

func TestL1MsgCorrectOrder(t *testing.T) {
	assert := assert.New(t)
	var (
		engine      consensus.Engine
		chainConfig *params.ChainConfig
		db          = rawdb.NewMemoryDatabase()
	)
	chainConfig = params.AllCliqueProtocolChanges
	chainConfig.Clique = &params.CliqueConfig{Period: 1, Epoch: 30000}
	chainConfig.Scroll.FeeVaultAddress = &common.Address{}
	engine = clique.New(chainConfig.Clique, db)

	maxTxPerBlock := 4
	chainConfig.Scroll.MaxTxPerBlock = &maxTxPerBlock
	chainConfig.Scroll.L1Config = &params.L1Config{
		NumL1MessagesPerBlock: 10,
	}

	// Insert 3 l1msgs
	l1msgs := []types.L1MessageTx{
		{QueueIndex: 0, Gas: 21016, To: &common.Address{3}, Data: []byte{0x01}, Sender: common.Address{4}},
		{QueueIndex: 1, Gas: 21016, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{2}},
		{QueueIndex: 2, Gas: 21016, To: &common.Address{3}, Data: []byte{0x01}, Sender: common.Address{4}}}
	rawdb.WriteL1Messages(db, l1msgs)

	chainConfig.LondonBlock = big.NewInt(0)
	w, b := newTestWorker(t, chainConfig, engine, db, 0)
	defer w.close()

	// This test chain imports the mined blocks.
	b.genesis.MustCommit(db)
	chain, _ := core.NewBlockChain(db, nil, b.chain.Config(), engine, vm.Config{
		Debug:  true,
		Tracer: vm.NewStructLogger(&vm.LogConfig{EnableMemory: true, EnableReturnData: true})}, nil, nil)
	defer chain.Stop()

	// Wait for mined blocks.
	sub := w.mux.Subscribe(core.NewMinedBlockEvent{})
	defer sub.Unsubscribe()

	// Insert local tx
	b.txPool.AddLocal(b.newRandomTx(true))

	// Start mining!
	w.start()

	select {
	case ev := <-sub.Chan():
		block := ev.Data.(core.NewMinedBlockEvent).Block
		if _, err := chain.InsertChain([]*types.Block{block}); err != nil {
			t.Fatalf("failed to insert new mined block %d: %v", block.NumberU64(), err)
		}
		assert.Equal(4, len(block.Transactions()))
		assert.True(block.Transactions()[0].IsL1MessageTx() && block.Transactions()[1].IsL1MessageTx() && block.Transactions()[2].IsL1MessageTx())
		assert.Equal(uint64(0), block.Transactions()[0].AsL1MessageTx().QueueIndex)
		assert.Equal(uint64(1), block.Transactions()[1].AsL1MessageTx().QueueIndex)
		assert.Equal(uint64(2), block.Transactions()[2].AsL1MessageTx().QueueIndex)
	case <-time.After(3 * time.Second):
		t.Fatalf("timeout")
	}
}

func l1MessageTest(t *testing.T, msgs []types.L1MessageTx, withL2Tx bool, callback func(i int, block *types.Block, db ethdb.Database, w *worker) bool) {
	var (
		engine      consensus.Engine
		chainConfig *params.ChainConfig
		db          = rawdb.NewMemoryDatabase()
	)
	rawdb.WriteL1Messages(db, msgs)

	chainConfig = params.AllCliqueProtocolChanges
	chainConfig.Clique = &params.CliqueConfig{Period: 1, Epoch: 30000}
	engine = clique.New(chainConfig.Clique, db)
	maxTxPerBlock := 4
	chainConfig.Scroll.MaxTxPerBlock = &maxTxPerBlock

	maxPayload := 1024
	chainConfig.Scroll.MaxTxPayloadBytesPerBlock = &maxPayload
	chainConfig.Scroll.L1Config = &params.L1Config{
		NumL1MessagesPerBlock: 3,
	}

	chainConfig.LondonBlock = big.NewInt(0)
	w, b := newTestWorker(t, chainConfig, engine, db, 0)
	defer w.close()

	// This test chain imports the mined blocks.
	b.genesis.MustCommit(db)
	chain, _ := core.NewBlockChain(db, nil, b.chain.Config(), engine, vm.Config{
		Debug:  true,
		Tracer: vm.NewStructLogger(&vm.LogConfig{EnableMemory: true, EnableReturnData: true})}, nil, nil)
	defer chain.Stop()

	// Wait for mined blocks.
	sub := w.mux.Subscribe(core.NewMinedBlockEvent{})
	defer sub.Unsubscribe()

	if withL2Tx {
		b.txPool.AddLocal(b.newRandomTx(false))
	}

	// Start mining!
	w.start()

	// call once before first block
	callback(0, nil, db, w)

	// timeout for all blocks
	globalTimeout := time.After(3 * time.Second)

	for ii := 1; true; ii++ {
		select {
		case <-globalTimeout:
			t.Fatalf("timeout")
		default:
		}

		select {
		case ev := <-sub.Chan():
			block := ev.Data.(core.NewMinedBlockEvent).Block

			if done := callback(ii, block, db, w); done {
				return
			}

		// timeout for one block
		case <-time.After(3 * time.Second):
			t.Fatalf("timeout")
		}
	}
}

func TestL1SingleMessageOverGasLimit(t *testing.T) {
	assert := assert.New(t)

	msgs := []types.L1MessageTx{
		{QueueIndex: 0, Gas: 10000000, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{2}}, // over gas limit
		{QueueIndex: 1, Gas: 21016, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{2}},    // same sender
		{QueueIndex: 2, Gas: 21016, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{3}},    // different sender
	}

	l1MessageTest(t, msgs, false, func(blockNum int, block *types.Block, db ethdb.Database, w *worker) bool {
		switch blockNum {
		case 0:
			return false
		case 1:
			// skip #0, include #1 and #2
			assert.Equal(2, len(block.Transactions()))

			assert.True(block.Transactions()[0].IsL1MessageTx())
			assert.Equal(uint64(1), block.Transactions()[0].AsL1MessageTx().QueueIndex)
			assert.True(block.Transactions()[1].IsL1MessageTx())
			assert.Equal(uint64(2), block.Transactions()[1].AsL1MessageTx().QueueIndex)

			// db is updated correctly
			queueIndex := rawdb.ReadFirstQueueIndexNotInL2Block(db, block.Hash())
			assert.NotNil(queueIndex)
			assert.Equal(uint64(3), *queueIndex)

			return true
		default:
			return true
		}
	})
}

func TestL1CombinedMessagesOverGasLimit(t *testing.T) {
	assert := assert.New(t)

	// message #0 is over the gas limit
	// we should skip #0 but not #1 and #2
	msgs := []types.L1MessageTx{
		{QueueIndex: 0, Gas: 4000000, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{2}},
		{QueueIndex: 1, Gas: 4000000, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{2}}, // same sender
		{QueueIndex: 2, Gas: 21016, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{3}},   // different sender
	}

	l1MessageTest(t, msgs, false, func(blockNum int, block *types.Block, db ethdb.Database, w *worker) bool {
		switch blockNum {
		case 0:
			return false
		case 1:
			// block #1 only includes 1 message
			assert.Equal(1, len(block.Transactions()))
			assert.True(block.Transactions()[0].IsL1MessageTx())
			assert.Equal(uint64(0), block.Transactions()[0].AsL1MessageTx().QueueIndex)

			// db is updated correctly
			queueIndex := rawdb.ReadFirstQueueIndexNotInL2Block(db, block.Hash())
			assert.NotNil(queueIndex)
			assert.Equal(uint64(1), *queueIndex)
			return false
		case 2:
			// block #2 includes the other 2 messages
			assert.Equal(2, len(block.Transactions()))
			assert.True(block.Transactions()[0].IsL1MessageTx())
			assert.Equal(uint64(1), block.Transactions()[0].AsL1MessageTx().QueueIndex)
			assert.True(block.Transactions()[1].IsL1MessageTx())
			assert.Equal(uint64(2), block.Transactions()[1].AsL1MessageTx().QueueIndex)

			// db is updated correctly
			queueIndex := rawdb.ReadFirstQueueIndexNotInL2Block(db, block.Hash())
			assert.NotNil(queueIndex)
			assert.Equal(uint64(3), *queueIndex)
			return true
		default:
			return true
		}
	})
}

func TestLargeL1MessageSkipPayloadCheck(t *testing.T) {
	assert := assert.New(t)

	// message #0 is over the L2 block payload size limit
	msgs := []types.L1MessageTx{
		{QueueIndex: 0, Gas: 25100, To: &common.Address{1}, Data: make([]byte, 1025), Sender: common.Address{2}},
		{QueueIndex: 1, Gas: 21016, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{2}}, // same sender
		{QueueIndex: 2, Gas: 21016, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{3}}, // different sender
	}

	l1MessageTest(t, msgs, true, func(blockNum int, block *types.Block, db ethdb.Database, w *worker) bool {
		switch blockNum {
		case 0:
			return false
		case 1:
			// include #0, #1 and #2 + one L2 tx
			assert.Equal(4, len(block.Transactions()))

			assert.True(block.Transactions()[0].IsL1MessageTx())
			assert.Equal(uint64(0), block.Transactions()[0].AsL1MessageTx().QueueIndex)
			assert.True(block.Transactions()[1].IsL1MessageTx())
			assert.Equal(uint64(1), block.Transactions()[1].AsL1MessageTx().QueueIndex)
			assert.True(block.Transactions()[2].IsL1MessageTx())
			assert.Equal(uint64(2), block.Transactions()[2].AsL1MessageTx().QueueIndex)

			// since L1 messages do not count against the block size limit,
			// we can include additional L2 transaction
			assert.False(block.Transactions()[3].IsL1MessageTx())

			// db is updated correctly
			queueIndex := rawdb.ReadFirstQueueIndexNotInL2Block(db, block.Hash())
			assert.NotNil(queueIndex)
			assert.Equal(uint64(3), *queueIndex)

			return true
		default:
			return true
		}
	})
}

func TestSkipMessageWithStrangeError(t *testing.T) {
	assert := assert.New(t)

	// message #0 is skipped because of `Value`
	// TODO: trigger skipping in some other way after this behaviour is changed
	msgs := []types.L1MessageTx{
		{QueueIndex: 0, Gas: 25100, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{2}, Value: big.NewInt(1)},
		{QueueIndex: 1, Gas: 21016, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{2}},
		{QueueIndex: 2, Gas: 21016, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{3}},
	}

	l1MessageTest(t, msgs, false, func(blockNum int, block *types.Block, db ethdb.Database, w *worker) bool {
		switch blockNum {
		case 0:
			return false
		case 1:
			// skip #0, include #1 and #2
			assert.Equal(2, len(block.Transactions()))
			assert.True(block.Transactions()[0].IsL1MessageTx())

			assert.True(block.Transactions()[0].IsL1MessageTx())
			assert.Equal(uint64(1), block.Transactions()[0].AsL1MessageTx().QueueIndex)
			assert.True(block.Transactions()[1].IsL1MessageTx())
			assert.Equal(uint64(2), block.Transactions()[1].AsL1MessageTx().QueueIndex)

			// db is updated correctly
			queueIndex := rawdb.ReadFirstQueueIndexNotInL2Block(db, block.Hash())
			assert.NotNil(queueIndex)
			assert.Equal(uint64(3), *queueIndex)

			return true
		default:
			return false
		}
	})
}

func TestSkipAllL1MessagesInBlock(t *testing.T) {
	assert := assert.New(t)

	// messages are skipped because of `Value`
	// TODO: trigger skipping in some other way after this behaviour is changed
	msgs := []types.L1MessageTx{
		{QueueIndex: 0, Gas: 25100, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{2}, Value: big.NewInt(1)},
		{QueueIndex: 1, Gas: 21016, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{2}, Value: big.NewInt(1)},
		{QueueIndex: 2, Gas: 21016, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{3}, Value: big.NewInt(1)},
	}

	l1MessageTest(t, msgs, true, func(blockNum int, block *types.Block, db ethdb.Database, w *worker) bool {
		switch blockNum {
		case 0:
			return false
		case 1:
			// skip all 3 L1 messages, include 1 L2 tx
			assert.Equal(1, len(block.Transactions()))
			assert.False(block.Transactions()[0].IsL1MessageTx())

			// db is updated correctly
			// note: this should return 0 but on the signer we store 3 instead so
			// that we do not process the same messages again for the next block.
			queueIndex := rawdb.ReadFirstQueueIndexNotInL2Block(db, block.Hash())
			assert.NotNil(queueIndex)
			assert.Equal(uint64(3), *queueIndex)

			return true
		default:
			return true
		}
	})
}

func TestOversizedTxThenNormal(t *testing.T) {
	assert := assert.New(t)

	msgs := []types.L1MessageTx{
		{QueueIndex: 0, Gas: 25100, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{2}},
		{QueueIndex: 1, Gas: 21016, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{2}},
		{QueueIndex: 2, Gas: 21016, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{3}},
	}

	l1MessageTest(t, msgs, false, func(blockNum int, block *types.Block, db ethdb.Database, w *worker) bool {
		switch blockNum {
		case 0:
			// schedule to skip 2nd call to ccc
			w.getCCC().ScheduleError(2, circuitcapacitychecker.ErrBlockRowConsumptionOverflow)
			return false
		case 1:
			// include #0, fail on #1, then seal the block
			assert.Equal(1, len(block.Transactions()))

			assert.True(block.Transactions()[0].IsL1MessageTx())
			assert.Equal(uint64(0), block.Transactions()[0].AsL1MessageTx().QueueIndex)

			// db is updated correctly
			queueIndex := rawdb.ReadFirstQueueIndexNotInL2Block(db, block.Hash())
			assert.NotNil(queueIndex)
			assert.Equal(uint64(1), *queueIndex)

			// schedule to skip next call to ccc
			w.getCCC().ScheduleError(1, circuitcapacitychecker.ErrBlockRowConsumptionOverflow)

			return false
		case 2:
			// skip #1, include #2, then seal the block
			assert.Equal(1, len(block.Transactions()))

			assert.True(block.Transactions()[0].IsL1MessageTx())
			assert.Equal(uint64(2), block.Transactions()[0].AsL1MessageTx().QueueIndex)

			// db is updated correctly
			queueIndex := rawdb.ReadFirstQueueIndexNotInL2Block(db, block.Hash())
			assert.NotNil(queueIndex)
			assert.Equal(uint64(3), *queueIndex)

			return true
		default:
			return true
		}
	})
}

func TestPrioritizeOverflowTx(t *testing.T) {
	assert := assert.New(t)

	var (
		chainConfig = params.AllCliqueProtocolChanges
		db          = rawdb.NewMemoryDatabase()
	)

	chainConfig.Clique = &params.CliqueConfig{Period: 1, Epoch: 30000}
	chainConfig.LondonBlock = big.NewInt(0)
	chainConfig.Scroll.FeeVaultAddress = &common.Address{}
	engine := clique.New(chainConfig.Clique, db)

	w, b := newTestWorker(t, chainConfig, engine, db, 0)
	defer w.close()

	// This test chain imports the mined blocks.
	db2 := rawdb.NewMemoryDatabase()
	b.genesis.MustCommit(db2)
	chain, _ := core.NewBlockChain(db2, nil, b.chain.Config(), engine, vm.Config{
		Debug:  true,
		Tracer: vm.NewStructLogger(&vm.LogConfig{EnableMemory: true, EnableReturnData: true})}, nil, nil)
	defer chain.Stop()

	// Wait for mined blocks.
	sub := w.mux.Subscribe(core.NewMinedBlockEvent{})
	defer sub.Unsubscribe()

	// Define 3 transactions:
	// A --> B (nonce: 0, gas: 20)
	tx0, _ := types.SignTx(types.NewTransaction(b.txPool.Nonce(testBankAddress), testUserAddress, big.NewInt(100000000000000000), params.TxGas, big.NewInt(20*params.InitialBaseFee), nil), types.HomesteadSigner{}, testBankKey)
	// A --> B (nonce: 1, gas: 5)
	tx1, _ := types.SignTx(types.NewTransaction(b.txPool.Nonce(testBankAddress)+1, testUserAddress, big.NewInt(0), params.TxGas, big.NewInt(5*params.InitialBaseFee), nil), types.HomesteadSigner{}, testBankKey)
	// B --> A (nonce: 0, gas: 20)
	tx2, _ := types.SignTx(types.NewTransaction(b.txPool.Nonce(testUserAddress), testBankAddress, big.NewInt(0), params.TxGas, big.NewInt(20*params.InitialBaseFee), nil), types.HomesteadSigner{}, testUserKey)
	// B --> A (nonce: 1, gas: 20)
	tx3, _ := types.SignTx(types.NewTransaction(b.txPool.Nonce(testUserAddress)+1, testBankAddress, big.NewInt(0), params.TxGas, big.NewInt(20*params.InitialBaseFee), nil), types.HomesteadSigner{}, testUserKey)
	// B --> A (nonce: 2, gas: 20)
	tx4, _ := types.SignTx(types.NewTransaction(b.txPool.Nonce(testUserAddress)+2, testBankAddress, big.NewInt(0), params.TxGas, big.NewInt(20*params.InitialBaseFee), nil), types.HomesteadSigner{}, testUserKey)
	// A --> B (nonce: 2, gas: 5)
	tx5, _ := types.SignTx(types.NewTransaction(b.txPool.Nonce(testBankAddress)+2, testUserAddress, big.NewInt(0), params.TxGas, big.NewInt(5*params.InitialBaseFee), nil), types.HomesteadSigner{}, testBankKey)

	// Process 2 transactions with gas order: tx0 > tx1, tx1 will overflow.
	b.txPool.AddRemotesSync([]*types.Transaction{tx0, tx1})
	w.getCCC().ScheduleError(2, circuitcapacitychecker.ErrBlockRowConsumptionOverflow)
	w.start()

	select {
	case ev := <-sub.Chan():
		block := ev.Data.(core.NewMinedBlockEvent).Block
		assert.Equal(1, len(block.Transactions()))
		assert.Equal(tx0.Hash(), block.Transactions()[0].Hash())
		if _, err := chain.InsertChain([]*types.Block{block}); err != nil {
			t.Fatalf("failed to insert new mined block %d: %v", block.NumberU64(), err)
		}
	case <-time.After(3 * time.Second): // Worker needs 1s to include new changes.
		t.Fatalf("timeout")
	}

	// Process 2 transactions with gas order: tx2 > tx1,
	// but we will prioritize tx1.
	b.txPool.AddRemotesSync([]*types.Transaction{tx2})

	select {
	case ev := <-sub.Chan():
		w.stop()
		block := ev.Data.(core.NewMinedBlockEvent).Block
		assert.Equal(2, len(block.Transactions()))
		// note: txs are not included according to their gas fee order
		assert.Equal(tx1.Hash(), block.Transactions()[0].Hash())
		assert.Equal(tx2.Hash(), block.Transactions()[1].Hash())
		if _, err := chain.InsertChain([]*types.Block{block}); err != nil {
			t.Fatalf("failed to insert new mined block %d: %v", block.NumberU64(), err)
		}
	case <-time.After(3 * time.Second): // Worker needs 1s to include new changes.
		t.Fatalf("timeout")
	}

	w.getCCC().Skip(tx4.Hash(), circuitcapacitychecker.ErrBlockRowConsumptionOverflow)
	assert.Equal([]error{nil, nil, nil}, b.txPool.AddRemotesSync([]*types.Transaction{tx3, tx4, tx5}))

	w.start()
	expectedTxns := []common.Hash{tx3.Hash(), tx5.Hash()}
	for i := 0; i < 2; i++ {
		select {
		case ev := <-sub.Chan():
			block := ev.Data.(core.NewMinedBlockEvent).Block
			assert.Equal(1, len(block.Transactions()))
			assert.Equal(expectedTxns[i], block.Transactions()[0].Hash())
			if _, err := chain.InsertChain([]*types.Block{block}); err != nil {
				t.Fatalf("failed to insert new mined block %d: %v", block.NumberU64(), err)
			}
		case <-time.After(3 * time.Second): // Worker needs 1s to include new changes.
			t.Fatalf("timeout")
		}
	}
	assert.False(b.txPool.Has(tx4.Hash()))
}

func TestSkippedTransactionDatabaseEntries(t *testing.T) {
	assert := assert.New(t)

	msgs := []types.L1MessageTx{
		{QueueIndex: 0, Gas: 10000000, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{2}}, // over gas limit
		{QueueIndex: 1, Gas: 21016, To: &common.Address{1}, Data: []byte{0x01}, Sender: common.Address{2}},
	}

	l1MessageTest(t, msgs, false, func(blockNum int, block *types.Block, db ethdb.Database, w *worker) bool {
		switch blockNum {
		case 0:
			return false
		case 1:
			// skip #0, include #1
			assert.Equal(1, len(block.Transactions()))

			assert.True(block.Transactions()[0].IsL1MessageTx())
			assert.Equal(uint64(1), block.Transactions()[0].AsL1MessageTx().QueueIndex)

			// db is updated correctly
			queueIndex := rawdb.ReadFirstQueueIndexNotInL2Block(db, block.Hash())
			assert.NotNil(queueIndex)

			assert.Equal(uint64(2), *queueIndex)

			l1msg := rawdb.ReadL1Message(db, 0)
			assert.NotNil(l1msg)
			hash := types.NewTx(l1msg).Hash()

			stx := rawdb.ReadSkippedTransaction(db, hash)
			assert.NotNil(stx)
			assert.True(stx.Tx.IsL1MessageTx())
			assert.Equal(uint64(0), stx.Tx.AsL1MessageTx().QueueIndex)
			assert.Equal("gas limit reached", stx.Reason)
			assert.Equal(block.NumberU64(), stx.BlockNumber)
			assert.Nil(stx.BlockHash)

			numSkipped := rawdb.ReadNumSkippedTransactions(db)
			assert.Equal(uint64(1), numSkipped)

			hash2 := rawdb.ReadSkippedTransactionHash(db, 0)
			assert.NotNil(hash2)
			assert.Equal(&hash, hash2)

			// iterator API
			it := rawdb.IterateSkippedTransactionsFrom(db, 0)
			hasMore := it.Next()
			assert.True(hasMore)
			assert.Equal(uint64(0), it.Index())
			hash3 := it.TransactionHash()
			assert.Equal(hash, hash3)
			hasMore = it.Next()
			assert.False(hasMore)
			return true
		default:
			return true
		}
	})
}

func TestSealBlockAfterCliquePeriod(t *testing.T) {
	assert := assert.New(t)
	var (
		engine      consensus.Engine
		chainConfig *params.ChainConfig
		db          = rawdb.NewMemoryDatabase()
	)
	chainConfig = params.AllCliqueProtocolChanges
	chainConfig.Clique = &params.CliqueConfig{Period: 1, Epoch: 30000}
	chainConfig.Scroll.FeeVaultAddress = &common.Address{}
	engine = clique.New(chainConfig.Clique, db)
	w, b := newTestWorker(t, chainConfig, engine, db, 0)
	defer w.close()

	// This test chain imports the mined blocks.
	b.genesis.MustCommit(db)
	chain, _ := core.NewBlockChain(db, nil, b.chain.Config(), engine, vm.Config{
		Debug:  true,
		Tracer: vm.NewStructLogger(&vm.LogConfig{EnableMemory: true, EnableReturnData: true})}, nil, nil)
	defer chain.Stop()

	// Add artificial delay to transaction processing.
	w.beforeTxHook = func() {
		time.Sleep(time.Duration(chainConfig.Clique.Period) * 1 * time.Second)
	}

	// Wait for mined blocks.
	sub := w.mux.Subscribe(core.NewMinedBlockEvent{})
	defer sub.Unsubscribe()

	// Insert 2 non-l1msg txs
	b.txPool.AddLocal(b.newRandomTx(true))
	b.txPool.AddLocal(b.newRandomTx(false))

	// Start mining!
	w.start()

	select {
	case ev := <-sub.Chan():
		block := ev.Data.(core.NewMinedBlockEvent).Block
		if _, err := chain.InsertChain([]*types.Block{block}); err != nil {
			t.Fatalf("failed to insert new mined block %d: %v", block.NumberU64(), err)
		}
		assert.Equal(1, len(block.Transactions())) // only packed 1 tx, not 2
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout")
	}

	select {
	case ev := <-sub.Chan():
		block := ev.Data.(core.NewMinedBlockEvent).Block
		if _, err := chain.InsertChain([]*types.Block{block}); err != nil {
			t.Fatalf("failed to insert new mined block %d: %v", block.NumberU64(), err)
		}
		assert.Equal(1, len(block.Transactions()))
	case <-time.After(5 * time.Second):
		t.Fatalf("timeout")
	}
}

func TestPending(t *testing.T) {
	var (
		engine      consensus.Engine
		chainConfig *params.ChainConfig
		db          = rawdb.NewMemoryDatabase()
	)
	chainConfig = params.AllCliqueProtocolChanges
	chainConfig.Clique = &params.CliqueConfig{Period: 1, Epoch: 30000}
	chainConfig.Scroll.FeeVaultAddress = &common.Address{}
	engine = clique.New(chainConfig.Clique, db)
	w, b := newTestWorker(t, chainConfig, engine, db, 0)
	defer w.close()

	// This test chain imports the mined blocks.
	b.genesis.MustCommit(db)
	chain, _ := core.NewBlockChain(db, nil, b.chain.Config(), engine, vm.Config{
		Debug:  true,
		Tracer: vm.NewStructLogger(&vm.LogConfig{EnableMemory: true, EnableReturnData: true})}, nil, nil)
	defer chain.Stop()

	// Define 3 transactions:
	// A --> B (nonce: 0, gas: 20)
	tx0, _ := types.SignTx(types.NewTransaction(b.txPool.Nonce(testBankAddress), testUserAddress, big.NewInt(100000000000000000), params.TxGas, big.NewInt(20*params.InitialBaseFee), nil), types.HomesteadSigner{}, testBankKey)
	// A --> B (nonce: 1, gas: 5)
	tx1, _ := types.SignTx(types.NewTransaction(b.txPool.Nonce(testBankAddress)+1, testUserAddress, big.NewInt(0), params.TxGas, big.NewInt(5*params.InitialBaseFee), nil), types.HomesteadSigner{}, testBankKey)
	// B --> A (nonce: 0, gas: 20)
	tx2, _ := types.SignTx(types.NewTransaction(b.txPool.Nonce(testUserAddress), testBankAddress, big.NewInt(0), params.TxGas, big.NewInt(20*params.InitialBaseFee), nil), types.HomesteadSigner{}, testUserKey)
	// B --> A (nonce: 1, gas: 20)
	tx3, _ := types.SignTx(types.NewTransaction(b.txPool.Nonce(testUserAddress)+1, testBankAddress, big.NewInt(0), params.TxGas, big.NewInt(20*params.InitialBaseFee), nil), types.HomesteadSigner{}, testUserKey)
	// B --> A (nonce: 2, gas: 20)
	tx4, _ := types.SignTx(types.NewTransaction(b.txPool.Nonce(testUserAddress)+2, testBankAddress, big.NewInt(0), params.TxGas, big.NewInt(20*params.InitialBaseFee), nil), types.HomesteadSigner{}, testUserKey)
	// A --> B (nonce: 2, gas: 5)
	tx5, _ := types.SignTx(types.NewTransaction(b.txPool.Nonce(testBankAddress)+2, testUserAddress, big.NewInt(0), params.TxGas, big.NewInt(5*params.InitialBaseFee), nil), types.HomesteadSigner{}, testBankKey)

	b.txPool.AddRemotesSync([]*types.Transaction{tx0, tx1, tx2, tx3, tx4, tx5})
	// start building pending block
	w.startCh <- struct{}{}

	time.Sleep(time.Second)
	pending := w.pendingBlock()
	assert.NotNil(t, pending)
	assert.NotEmpty(t, pending.Transactions())
}
