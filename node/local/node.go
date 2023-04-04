package local

import (
	"context"
	"encoding/hex"
	"fmt"
	"os"
	"sort"

	"github.com/cosmos/cosmos-sdk/codec"

	cfg "github.com/cometbft/cometbft/config"
	cs "github.com/cometbft/cometbft/consensus"
	"github.com/cometbft/cometbft/evidence"
	"github.com/cometbft/cometbft/libs/log"
	bftmath "github.com/cometbft/cometbft/libs/math"
	bftquery "github.com/cometbft/cometbft/libs/pubsub/query"
	"github.com/cometbft/cometbft/privval"
	"github.com/cometbft/cometbft/proxy"
	bftstate "github.com/cometbft/cometbft/state"
	"github.com/cometbft/cometbft/state/indexer"
	blockidxkv "github.com/cometbft/cometbft/state/indexer/block/kv"
	blockidxnull "github.com/cometbft/cometbft/state/indexer/block/null"
	"github.com/cometbft/cometbft/state/txindex"
	"github.com/cometbft/cometbft/state/txindex/kv"
	"github.com/cometbft/cometbft/state/txindex/null"
	"github.com/cometbft/cometbft/store"
	bfttypes "github.com/cometbft/cometbft/types"
	"github.com/cosmos/cosmos-sdk/client"
	"github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/spf13/viper"

	"github.com/forbole/juno/v4/node"
	"github.com/forbole/juno/v4/types"

	"path"
	"time"

	dbm "github.com/cometbft/cometbft-db"
	constypes "github.com/cometbft/cometbft/consensus/types"
	bftjson "github.com/cometbft/cometbft/libs/json"
	bftnode "github.com/cometbft/cometbft/node"
	bftcoretypes "github.com/cometbft/cometbft/rpc/core/types"
)

const (
	// see README
	defaultPerPage = 30
	maxPerPage     = 100
)

var (
	_ node.Node = &Node{}
)

// Node represents the node implementation that uses a local node
type Node struct {
	ctx      context.Context
	codec    codec.Codec
	txConfig client.TxConfig

	// config
	tmCfg      *cfg.Config
	genesisDoc *bfttypes.GenesisDoc

	// services
	eventBus       *bfttypes.EventBus
	stateStore     bftstate.Store
	blockStore     *store.BlockStore
	consensusState *cs.State
	txIndexer      txindex.TxIndexer
	blockIndexer   indexer.BlockIndexer
}

// NewNode returns a new Node instance
func NewNode(config *Details, txConfig client.TxConfig, codec codec.Codec) (*Node, error) {
	// Load the config
	viper.SetConfigFile(path.Join(config.Home, "config", "config.yaml"))
	tmCfg, err := ParseConfig()
	if err != nil {
		return nil, err
	}
	tmCfg.SetRoot(config.Home)

	// Build the local node
	dbProvider := bftnode.DefaultDBProvider
	genesisDocProvider := bftnode.DefaultGenesisDocProviderFunc(tmCfg)
	logger := log.NewTMLogger(log.NewSyncWriter(os.Stdout)).With("module", "explorer")
	clientCreator := proxy.DefaultClientCreator(tmCfg.ProxyApp, tmCfg.ABCI, tmCfg.DBDir())
	metricsProvider := bftnode.DefaultMetricsProvider(tmCfg.Instrumentation)

	privval.LoadOrGenFilePV(tmCfg.PrivValidatorKeyFile(), tmCfg.PrivValidatorStateFile())
	proxy.DefaultClientCreator(tmCfg.ProxyApp, tmCfg.ABCI, tmCfg.DBDir())

	blockStore, stateDB, err := initDBs(tmCfg, dbProvider)
	if err != nil {
		return nil, err
	}

	stateStore := bftstate.NewStore(stateDB, bftstate.StoreOptions{
		DiscardABCIResponses: false,
	})

	_, genDoc, err := bftnode.LoadStateFromDBOrGenesisDocProvider(stateDB, genesisDocProvider)
	if err != nil {
		return nil, err
	}

	eventBus, err := createAndStartEventBus(logger)
	if err != nil {
		return nil, err
	}

	_, txIndexer, blockIndexer, err := createAndStartIndexerService(tmCfg, dbProvider, eventBus, logger)
	if err != nil {
		return nil, err
	}

	state, err := stateStore.Load()
	if err != nil {
		return nil, err
	}

	csMetrics, _, _, smMetrics, proxyMetrics := metricsProvider(genDoc.ChainID)

	proxyApp := proxy.NewAppConns(clientCreator, proxyMetrics)

	evidenceDB, err := dbProvider(&bftnode.DBContext{ID: "evidence", Config: tmCfg})
	if err != nil {
		return nil, err
	}

	evidencePool, err := evidence.NewPool(evidenceDB, bftstate.NewStore(stateDB, bftstate.StoreOptions{
		DiscardABCIResponses: false,
	}), blockStore)
	if err != nil {
		return nil, err
	}

	blockExec := bftstate.NewBlockExecutor(
		stateStore,
		logger.With("module", "state"),
		proxyApp.Consensus(),
		nil,
		evidencePool,
		bftstate.BlockExecutorWithMetrics(smMetrics),
	)

	consensusState := cs.NewState(
		tmCfg.Consensus,
		state.Copy(),
		blockExec,
		blockStore,
		nil,
		evidencePool,
		cs.StateMetrics(csMetrics),
	)

	return &Node{
		ctx:      context.Background(),
		codec:    codec,
		txConfig: txConfig,

		tmCfg:      tmCfg,
		genesisDoc: genDoc,

		eventBus:       eventBus,
		stateStore:     stateStore,
		consensusState: consensusState,
		blockStore:     blockStore,
		txIndexer:      txIndexer,
		blockIndexer:   blockIndexer,
	}, nil
}

func initDBs(config *cfg.Config, dbProvider bftnode.DBProvider) (blockStore *store.BlockStore, stateDB dbm.DB, err error) {
	var blockStoreDB dbm.DB
	blockStoreDB, err = dbProvider(&bftnode.DBContext{ID: "blockstore", Config: config})
	if err != nil {
		return
	}
	blockStore = store.NewBlockStore(blockStoreDB)

	stateDB, err = dbProvider(&bftnode.DBContext{ID: "state", Config: config})
	if err != nil {
		return
	}

	return
}

func createAndStartEventBus(logger log.Logger) (*bfttypes.EventBus, error) {
	eventBus := bfttypes.NewEventBus()
	eventBus.SetLogger(logger.With("module", "events"))
	if err := eventBus.Start(); err != nil {
		return nil, err
	}
	return eventBus, nil
}

func createAndStartIndexerService(
	config *cfg.Config,
	dbProvider bftnode.DBProvider,
	eventBus *bfttypes.EventBus,
	logger log.Logger,
) (*txindex.IndexerService, txindex.TxIndexer, indexer.BlockIndexer, error) {

	var (
		txIndexer    txindex.TxIndexer
		blockIndexer indexer.BlockIndexer
	)

	switch config.TxIndex.Indexer {
	case "kv":
		store, err := dbProvider(&bftnode.DBContext{ID: "tx_index", Config: config})
		if err != nil {
			return nil, nil, nil, err
		}

		txIndexer = kv.NewTxIndex(store)
		blockIndexer = blockidxkv.New(dbm.NewPrefixDB(store, []byte("block_events")))
	default:
		txIndexer = &null.TxIndex{}
		blockIndexer = &blockidxnull.BlockerIndexer{}
	}

	indexerService := txindex.NewIndexerService(txIndexer, blockIndexer, eventBus, false)
	indexerService.SetLogger(logger.With("module", "txindex"))

	if err := indexerService.Start(); err != nil {
		return nil, nil, nil, err
	}

	return indexerService, txIndexer, blockIndexer, nil
}

// latestHeight can be either latest committed or uncommitted (+1) height.
func (cp *Node) getHeight(latestHeight int64, heightPtr *int64) (int64, error) {
	if heightPtr != nil {
		height := *heightPtr
		if height <= 0 {
			return 0, fmt.Errorf("height must be greater than 0, but got %d", height)
		}
		if height > latestHeight {
			return 0, fmt.Errorf("height %d must be less than or equal to the current blockchain height %d",
				height, latestHeight)
		}
		base := cp.blockStore.Base()
		if height < base {
			return 0, fmt.Errorf("height %d is not available, lowest height is %d",
				height, base)
		}
		return height, nil
	}
	return latestHeight, nil
}

func validatePerPage(perPagePtr *int) int {
	if perPagePtr == nil { // no per_page parameter
		return defaultPerPage
	}

	perPage := *perPagePtr
	if perPage < 1 {
		return defaultPerPage
	} else if perPage > maxPerPage {
		return maxPerPage
	}
	return perPage
}

func validatePage(pagePtr *int, perPage, totalCount int) (int, error) {
	if perPage < 1 {
		panic(fmt.Sprintf("zero or negative perPage: %d", perPage))
	}

	if pagePtr == nil { // no page parameter
		return 1, nil
	}

	pages := ((totalCount - 1) / perPage) + 1
	if pages == 0 {
		pages = 1 // one page (even if it's empty)
	}
	page := *pagePtr
	if page <= 0 || page > pages {
		return 1, fmt.Errorf("page should be within [1, %d] range, given %d", pages, page)
	}

	return page, nil
}

func validateSkipCount(page, perPage int) int {
	skipCount := (page - 1) * perPage
	if skipCount < 0 {
		return 0
	}

	return skipCount
}

// Genesis implements node.Node
func (cp *Node) Genesis() (*bftcoretypes.ResultGenesis, error) {
	return &bftcoretypes.ResultGenesis{Genesis: cp.genesisDoc}, nil
}

// ConsensusState implements node.Node
func (cp *Node) ConsensusState() (*constypes.RoundStateSimple, error) {
	bz, err := cp.consensusState.GetRoundStateSimpleJSON()
	if err != nil {
		return nil, err
	}

	var data constypes.RoundStateSimple
	err = bftjson.Unmarshal(bz, &data)
	if err != nil {
		return nil, err
	}
	return &data, nil
}

// LatestHeight implements node.Node
func (cp *Node) LatestHeight() (int64, error) {
	return cp.blockStore.Height(), nil
}

// ChainID implements node.Node
func (cp *Node) ChainID() (string, error) {
	return cp.genesisDoc.ChainID, nil
}

// Validators implements node.Node
func (cp *Node) Validators(height int64) (*bftcoretypes.ResultValidators, error) {
	height, err := cp.getHeight(cp.blockStore.Height(), &height)
	if err != nil {
		return nil, err
	}

	valSet, err := cp.stateStore.LoadValidators(height)
	if err != nil {
		return nil, err
	}

	return &bftcoretypes.ResultValidators{
		BlockHeight: height,
		Validators:  valSet.Validators,
		Count:       len(valSet.Validators),
		Total:       len(valSet.Validators),
	}, nil
}

// Block implements node.Node
func (cp *Node) Block(height int64) (*bftcoretypes.ResultBlock, error) {
	height, err := cp.getHeight(cp.blockStore.Height(), &height)
	if err != nil {
		return nil, err
	}

	block := cp.blockStore.LoadBlock(height)
	blockMeta := cp.blockStore.LoadBlockMeta(height)
	if blockMeta == nil {
		return &bftcoretypes.ResultBlock{BlockID: bfttypes.BlockID{}, Block: block}, nil
	}
	return &bftcoretypes.ResultBlock{BlockID: blockMeta.BlockID, Block: block}, nil
}

// BlockResults implements node.Node
func (cp *Node) BlockResults(height int64) (*bftcoretypes.ResultBlockResults, error) {
	height, err := cp.getHeight(cp.blockStore.Height(), &height)
	if err != nil {
		return nil, err
	}

	results, err := cp.stateStore.LoadABCIResponses(height)
	if err != nil {
		return nil, err
	}

	return &bftcoretypes.ResultBlockResults{
		Height:                height,
		TxsResults:            results.DeliverTxs,
		BeginBlockEvents:      results.BeginBlock.Events,
		EndBlockEvents:        results.EndBlock.Events,
		ValidatorUpdates:      results.EndBlock.ValidatorUpdates,
		ConsensusParamUpdates: results.EndBlock.ConsensusParamUpdates,
	}, nil
}

// Tx implements node.Node
func (cp *Node) Tx(hash string) (*types.Tx, error) {
	// if index is disabled, return error
	if _, ok := cp.txIndexer.(*null.TxIndex); ok {
		return nil, fmt.Errorf("transaction indexing is disabled")
	}

	hashBz, err := hex.DecodeString(hash)
	if err != nil {
		return nil, err
	}

	r, err := cp.txIndexer.Get(hashBz)
	if err != nil {
		return nil, err
	}

	if r == nil {
		return nil, fmt.Errorf("tx %s not found", hash)
	}

	height := r.Height
	index := r.Index

	resTx := &bftcoretypes.ResultTx{
		Hash:     []byte(hash),
		Height:   height,
		Index:    index,
		TxResult: r.Result,
		Tx:       r.Tx,
	}

	resBlock, err := cp.Block(resTx.Height)
	if err != nil {
		return nil, err
	}

	txResponse, err := makeTxResult(cp.txConfig, resTx, resBlock)
	if err != nil {
		return nil, err
	}

	protoTx, ok := txResponse.Tx.GetCachedValue().(*tx.Tx)
	if !ok {
		return nil, fmt.Errorf("expected %T, got %T", tx.Tx{}, txResponse.Tx.GetCachedValue())
	}

	convTx, err := types.NewTx(txResponse, protoTx)
	if err != nil {
		return nil, fmt.Errorf("error converting transaction: %s", err.Error())
	}

	return convTx, nil
}

// Txs implements node.Node
func (cp *Node) Txs(block *bftcoretypes.ResultBlock) ([]*types.Tx, error) {
	txResponses := make([]*types.Tx, len(block.Block.Txs))
	for i, tmTx := range block.Block.Txs {
		txResponse, err := cp.Tx(fmt.Sprintf("%X", tmTx.Hash()))
		if err != nil {
			return nil, err
		}

		txResponses[i] = txResponse
	}

	return txResponses, nil
}

// TxSearch implements node.Node
func (cp *Node) TxSearch(query string, pagePtr *int, perPagePtr *int, orderBy string) (*bftcoretypes.ResultTxSearch, error) {
	q, err := bftquery.New(query)
	if err != nil {
		return nil, err
	}

	results, err := cp.txIndexer.Search(cp.ctx, q)
	if err != nil {
		return nil, err
	}

	// sort results (must be done before pagination)
	switch orderBy {
	case "desc":
		sort.Slice(results, func(i, j int) bool {
			if results[i].Height == results[j].Height {
				return results[i].Index > results[j].Index
			}
			return results[i].Height > results[j].Height
		})
	case "asc", "":
		sort.Slice(results, func(i, j int) bool {
			if results[i].Height == results[j].Height {
				return results[i].Index < results[j].Index
			}
			return results[i].Height < results[j].Height
		})
	default:
		return nil, fmt.Errorf("expected order_by to be either `asc` or `desc` or empty")
	}

	// paginate results
	totalCount := len(results)
	perPage := validatePerPage(perPagePtr)

	page, err := validatePage(pagePtr, perPage, totalCount)
	if err != nil {
		return nil, err
	}

	skipCount := validateSkipCount(page, perPage)
	pageSize := bftmath.MinInt(perPage, totalCount-skipCount)

	apiResults := make([]*bftcoretypes.ResultTx, 0, pageSize)
	for i := skipCount; i < skipCount+pageSize; i++ {
		r := results[i]

		var proof bfttypes.TxProof
		apiResults = append(apiResults, &bftcoretypes.ResultTx{
			Hash:     bfttypes.Tx(r.Tx).Hash(),
			Height:   r.Height,
			Index:    r.Index,
			TxResult: r.Result,
			Tx:       r.Tx,
			Proof:    proof,
		})
	}

	return &bftcoretypes.ResultTxSearch{Txs: apiResults, TotalCount: totalCount}, nil
}

// SubscribeEvents implements node.Node
func (cp *Node) SubscribeEvents(subscriber, query string) (<-chan bftcoretypes.ResultEvent, context.CancelFunc, error) {
	_, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	eventCh := make(<-chan bftcoretypes.ResultEvent)
	return eventCh, cancel, nil
}

// SubscribeNewBlocks implements node.Node
func (cp *Node) SubscribeNewBlocks(subscriber string) (<-chan bftcoretypes.ResultEvent, context.CancelFunc, error) {
	return cp.SubscribeEvents(subscriber, "tm.event = 'NewBlock'")
}

// Stop implements node.Node
func (cp *Node) Stop() {
}
