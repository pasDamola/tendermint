package blockchain_new

import (
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	abci "github.com/tendermint/tendermint/abci/types"
	cfg "github.com/tendermint/tendermint/config"
	cmn "github.com/tendermint/tendermint/libs/common"
	dbm "github.com/tendermint/tendermint/libs/db"
	"github.com/tendermint/tendermint/libs/log"
	"github.com/tendermint/tendermint/p2p"
	"github.com/tendermint/tendermint/proxy"
	sm "github.com/tendermint/tendermint/state"
	"github.com/tendermint/tendermint/types"
	tmtime "github.com/tendermint/tendermint/types/time"
)

var config *cfg.Config

func randGenesisDoc(numValidators int, randPower bool, minPower int64) (*types.GenesisDoc, []types.PrivValidator) {
	validators := make([]types.GenesisValidator, numValidators)
	privValidators := make([]types.PrivValidator, numValidators)
	for i := 0; i < numValidators; i++ {
		val, privVal := types.RandValidator(randPower, minPower)
		validators[i] = types.GenesisValidator{
			PubKey: val.PubKey,
			Power:  val.VotingPower,
		}
		privValidators[i] = privVal
	}
	sort.Sort(types.PrivValidatorsByAddress(privValidators))

	return &types.GenesisDoc{
		GenesisTime: tmtime.Now(),
		ChainID:     config.ChainID(),
		Validators:  validators,
	}, privValidators
}

func makeVote(header *types.Header, blockID types.BlockID, valset *types.ValidatorSet, privVal types.PrivValidator) *types.Vote {
	addr := privVal.GetPubKey().Address()
	idx, _ := valset.GetByAddress(addr)
	vote := &types.Vote{
		ValidatorAddress: addr,
		ValidatorIndex:   idx,
		Height:           header.Height,
		Round:            1,
		Timestamp:        tmtime.Now(),
		Type:             types.PrecommitType,
		BlockID:          blockID,
	}

	_ = privVal.SignVote(header.ChainID, vote)

	return vote
}

type BlockchainReactorPair struct {
	reactor *BlockchainReactor
	app     proxy.AppConns
}

func newBlockchainReactor(logger log.Logger, genDoc *types.GenesisDoc, privVals []types.PrivValidator, maxBlockHeight int64) BlockchainReactorPair {
	if len(privVals) != 1 {
		panic("only support one validator")
	}

	app := &testApp{}
	cc := proxy.NewLocalClientCreator(app)
	proxyApp := proxy.NewAppConns(cc)
	err := proxyApp.Start()
	if err != nil {
		panic(cmn.ErrorWrap(err, "error start app"))
	}

	blockDB := dbm.NewMemDB()
	stateDB := dbm.NewMemDB()
	blockStore := NewBlockStore(blockDB)

	state, err := sm.LoadStateFromDBOrGenesisDoc(stateDB, genDoc)
	if err != nil {
		panic(cmn.ErrorWrap(err, "error constructing state from genesis file"))
	}

	// Make the BlockchainReactor itself.
	// NOTE we have to create and commit the blocks first because
	// pool.height is determined from the store.
	fastSync := true
	blockExec := sm.NewBlockExecutor(dbm.NewMemDB(), log.TestingLogger(), proxyApp.Consensus(),
		sm.MockMempool{}, sm.MockEvidencePool{})

	// let's add some blocks in
	for blockHeight := int64(1); blockHeight <= maxBlockHeight; blockHeight++ {
		lastCommit := types.NewCommit(types.BlockID{}, nil)
		if blockHeight > 1 {
			lastBlockMeta := blockStore.LoadBlockMeta(blockHeight - 1)
			lastBlock := blockStore.LoadBlock(blockHeight - 1)

			vote := makeVote(&lastBlock.Header, lastBlockMeta.BlockID, state.Validators, privVals[0]).CommitSig()
			lastCommit = types.NewCommit(lastBlockMeta.BlockID, []*types.CommitSig{vote})
		}

		thisBlock := makeBlock(blockHeight, state, lastCommit)

		thisParts := thisBlock.MakePartSet(types.BlockPartSizeBytes)
		blockID := types.BlockID{Hash: thisBlock.Hash(), PartsHeader: thisParts.Header()}

		state, err = blockExec.ApplyBlock(state, blockID, thisBlock)
		if err != nil {
			panic(cmn.ErrorWrap(err, "error apply block"))
		}

		blockStore.SaveBlock(thisBlock, thisParts, lastCommit)
	}

	bcReactor := NewBlockchainReactor(state.Copy(), blockExec, blockStore, fastSync)
	bcReactor.SetLogger(logger.With("module", "blockchain"))

	return BlockchainReactorPair{bcReactor, proxyApp}
}

func TestNoBlockResponse(t *testing.T) {
	peerTimeout = 15 * time.Second
	maxRequestBatchSize = 200

	config = cfg.ResetTestRoot("blockchain_new_reactor_test")
	defer os.RemoveAll(config.RootDir)
	genDoc, privVals := randGenesisDoc(1, false, 30)

	maxBlockHeight := int64(500)

	reactorPairs := make([]BlockchainReactorPair, 2)

	logger1 := log.TestingLogger()
	reactorPairs[0] = newBlockchainReactor(logger1, genDoc, privVals, maxBlockHeight)
	logger2 := log.TestingLogger()
	reactorPairs[1] = newBlockchainReactor(logger2, genDoc, privVals, 0)

	p2p.MakeConnectedSwitches(config.P2P, 2, func(i int, s *p2p.Switch) *p2p.Switch {
		s.AddReactor("BLOCKCHAIN", reactorPairs[i].reactor)
		return s

	}, p2p.Connect2Switches)

	addr0 := reactorPairs[0].reactor.Switch.NodeInfo().ID()
	moduleName := fmt.Sprintf("blockchain-%v", addr0)
	reactorPairs[0].reactor.SetLogger(logger1.With("module", moduleName[:19]))
	addr1 := reactorPairs[1].reactor.Switch.NodeInfo().ID()
	moduleName = fmt.Sprintf("blockchain-%v", addr1)
	reactorPairs[1].reactor.SetLogger(logger1.With("module", moduleName[:19]))

	defer func() {
		for _, r := range reactorPairs {
			_ = r.reactor.Stop()
			_ = r.app.Stop()
		}
	}()

	tests := []struct {
		height   int64
		existent bool
	}{
		{maxBlockHeight + 2, false},
		{10, true},
		{1, true},
		{maxBlockHeight + 100, false},
	}

	for {
		if reactorPairs[1].reactor.fsm.IsFinished() {
			break
		}

		time.Sleep(10 * time.Millisecond)
	}

	assert.Equal(t, maxBlockHeight, reactorPairs[0].reactor.store.Height())

	for _, tt := range tests {
		block := reactorPairs[1].reactor.store.LoadBlock(tt.height)
		if tt.existent {
			assert.True(t, block != nil)
		} else {
			assert.True(t, block == nil)
		}
	}
}

// NOTE: This is too hard to test without
// an easy way to add test peer to switch
// or without significant refactoring of the module.
// Alternatively we could actually dial a TCP conn but
// that seems extreme.
func TestBadBlockStopsPeer(t *testing.T) {
	peerTimeout = 15 * time.Second
	maxRequestBatchSize = 32
	config = cfg.ResetTestRoot("blockchain_reactor_test")
	defer os.RemoveAll(config.RootDir)
	genDoc, privVals := randGenesisDoc(1, false, 30)

	maxBlockHeight := int64(148)

	otherChain := newBlockchainReactor(log.TestingLogger(), genDoc, privVals, maxBlockHeight)
	defer func() {
		_ = otherChain.reactor.Stop()
		_ = otherChain.app.Stop()
	}()

	reactorPairs := make([]BlockchainReactorPair, 4)

	var logger = make([]log.Logger, 4)

	for i := 0; i < 4; i++ {
		logger[i] = log.TestingLogger()
		height := int64(0)
		if i == 0 {
			height = maxBlockHeight
		}
		reactorPairs[i] = newBlockchainReactor(logger[i], genDoc, privVals, height)
	}

	switches := p2p.MakeConnectedSwitches(config.P2P, 4, func(i int, s *p2p.Switch) *p2p.Switch {
		s.AddReactor("BLOCKCHAIN", reactorPairs[i].reactor)
		return s

	}, p2p.Connect2Switches)

	for i := 0; i < 4; i++ {
		addr := reactorPairs[i].reactor.Switch.NodeInfo().ID()
		moduleName := fmt.Sprintf("blockchain-%v", addr)
		reactorPairs[i].reactor.SetLogger(logger[i].With("module", moduleName[:19]))
	}

	defer func() {
		for _, r := range reactorPairs {
			_ = r.reactor.Stop()
			_ = r.app.Stop()
		}
	}()

	for {
		if reactorPairs[3].reactor.fsm.IsFinished() || reactorPairs[3].reactor.Switch.Peers().Size() == 0 {
			break
		}

		time.Sleep(1 * time.Second)
	}

	//at this time, reactors[0-3] is the newest
	assert.Equal(t, 3, reactorPairs[1].reactor.Switch.Peers().Size())

	//mark reactorPairs[3] is an invalid peer
	reactorPairs[3].reactor.store = otherChain.reactor.store

	lastLogger := log.TestingLogger()
	lastReactorPair := newBlockchainReactor(lastLogger, genDoc, privVals, 0)
	reactorPairs = append(reactorPairs, lastReactorPair)

	switches = append(switches, p2p.MakeConnectedSwitches(config.P2P, 1, func(i int, s *p2p.Switch) *p2p.Switch {
		s.AddReactor("BLOCKCHAIN", reactorPairs[len(reactorPairs)-1].reactor)
		return s

	}, p2p.Connect2Switches)...)

	addr := lastReactorPair.reactor.Switch.NodeInfo().ID()
	moduleName := fmt.Sprintf("blockchain-%v", addr)
	lastReactorPair.reactor.SetLogger(lastLogger.With("module", moduleName[:19]))

	for i := 0; i < len(reactorPairs)-1; i++ {
		p2p.Connect2Switches(switches, i, len(reactorPairs)-1)
	}

	for {
		if lastReactorPair.reactor.fsm.IsFinished() || lastReactorPair.reactor.Switch.Peers().Size() == 0 {
			break
		}

		time.Sleep(1 * time.Second)
	}

	assert.True(t, lastReactorPair.reactor.Switch.Peers().Size() < len(reactorPairs)-1)
}

func setupReactors(
	numReactors int, maxBlockHeight int64,
	genDoc *types.GenesisDoc, privVals []types.PrivValidator) ([]BlockchainReactorPair, []*p2p.Switch) {

	defer os.RemoveAll(config.RootDir)

	reactorPairs := make([]BlockchainReactorPair, numReactors)

	var logger = make([]log.Logger, numReactors)

	for i := 0; i < numReactors; i++ {
		logger[i] = log.TestingLogger()
		height := int64(0)
		if i == 0 {
			height = maxBlockHeight
		}
		reactorPairs[i] = newBlockchainReactor(logger[i], genDoc, privVals, height)
	}

	switches := p2p.MakeConnectedSwitches(config.P2P, numReactors, func(i int, s *p2p.Switch) *p2p.Switch {
		s.AddReactor("BLOCKCHAIN", reactorPairs[i].reactor)
		return s

	}, p2p.Connect2Switches)

	for i := 0; i < numReactors; i++ {
		addr := reactorPairs[i].reactor.Switch.NodeInfo().ID()
		moduleName := fmt.Sprintf("blockchain-%v", addr)
		reactorPairs[i].reactor.SetLogger(logger[i].With("module", moduleName[:19]))
	}

	return reactorPairs, switches
}

func TestFastSyncMultiNode(t *testing.T) {

	numNodes := 8
	maxHeight := int64(2000)
	peerTimeout = 15 * time.Second
	maxRequestBatchSize = 128

	config = cfg.ResetTestRoot("blockchain_reactor_test")
	genDoc, privVals := randGenesisDoc(1, false, 30)

	reactorPairs, switches := setupReactors(numNodes, maxHeight, genDoc, privVals)

	defer func() {
		for _, r := range reactorPairs {
			_ = r.reactor.Stop()
			_ = r.app.Stop()
		}
	}()

	for {
		if reactorPairs[numNodes-1].reactor.fsm.IsFinished() || reactorPairs[numNodes-1].reactor.Switch.Peers().Size() == 0 {
			break
		}

		time.Sleep(1 * time.Second)
	}

	//at this time, reactors[0-3] are the newest
	assert.Equal(t, numNodes-1, reactorPairs[1].reactor.Switch.Peers().Size())

	lastLogger := log.TestingLogger()
	lastReactorPair := newBlockchainReactor(lastLogger, genDoc, privVals, 0)
	reactorPairs = append(reactorPairs, lastReactorPair)

	switches = append(switches, p2p.MakeConnectedSwitches(config.P2P, 1, func(i int, s *p2p.Switch) *p2p.Switch {
		s.AddReactor("BLOCKCHAIN", reactorPairs[len(reactorPairs)-1].reactor)
		return s

	}, p2p.Connect2Switches)...)

	addr := lastReactorPair.reactor.Switch.NodeInfo().ID()
	moduleName := fmt.Sprintf("blockchain-%v", addr)
	lastReactorPair.reactor.SetLogger(lastLogger.With("module", moduleName[:19]))

	start := time.Now()

	for i := 0; i < len(reactorPairs)-1; i++ {
		p2p.Connect2Switches(switches, i, len(reactorPairs)-1)
	}

	for {
		if lastReactorPair.reactor.fsm.IsFinished() || lastReactorPair.reactor.Switch.Peers().Size() == 0 {
			break
		}

		time.Sleep(1 * time.Second)
	}

	fmt.Println(time.Since(start))
	assert.True(t, lastReactorPair.reactor.Switch.Peers().Size() < len(reactorPairs))
	assert.Equal(t, lastReactorPair.reactor.fsm.pool.maxPeerHeight, lastReactorPair.reactor.fsm.pool.height)
}

//----------------------------------------------
// utility funcs

func makeTxs(height int64) (txs []types.Tx) {
	for i := 0; i < 10; i++ {
		txs = append(txs, types.Tx([]byte{byte(height), byte(i)}))
	}
	return txs
}

func makeBlock(height int64, state sm.State, lastCommit *types.Commit) *types.Block {
	block, _ := state.MakeBlock(height, makeTxs(height), lastCommit, nil, state.Validators.GetProposer().Address)
	return block
}

type testApp struct {
	abci.BaseApplication
}

var _ abci.Application = (*testApp)(nil)

func (app *testApp) Info(req abci.RequestInfo) (resInfo abci.ResponseInfo) {
	return abci.ResponseInfo{}
}

func (app *testApp) BeginBlock(req abci.RequestBeginBlock) abci.ResponseBeginBlock {
	return abci.ResponseBeginBlock{}
}

func (app *testApp) EndBlock(req abci.RequestEndBlock) abci.ResponseEndBlock {
	return abci.ResponseEndBlock{}
}

func (app *testApp) DeliverTx(tx []byte) abci.ResponseDeliverTx {
	return abci.ResponseDeliverTx{Tags: []cmn.KVPair{}}
}

func (app *testApp) CheckTx(tx []byte) abci.ResponseCheckTx {
	return abci.ResponseCheckTx{}
}

func (app *testApp) Commit() abci.ResponseCommit {
	return abci.ResponseCommit{}
}

func (app *testApp) Query(reqQuery abci.RequestQuery) (resQuery abci.ResponseQuery) {
	return
}
