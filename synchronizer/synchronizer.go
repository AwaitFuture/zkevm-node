package synchronizer

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/0xPolygonHermez/zkevm-node/etherman"
	"github.com/0xPolygonHermez/zkevm-node/log"
	"github.com/0xPolygonHermez/zkevm-node/state"
	"github.com/ethereum/go-ethereum/common"
	"github.com/jackc/pgx/v4"
)

// Synchronizer connects L1 and L2
type Synchronizer interface {
	Sync() error
	Stop()
}

// ClientSynchronizer connects L1 and L2
type ClientSynchronizer struct {
	etherMan          ethermanInterface
	state             stateInterface
	ctx               context.Context
	cancelCtx         context.CancelFunc
	genBlockNumber    uint64
	genesis           state.Genesis
	reorgBlockNumChan chan struct{}
	cfg               Config
}

// NewSynchronizer creates and initializes an instance of Synchronizer
func NewSynchronizer(
	ethMan ethermanInterface,
	st stateInterface,
	genBlockNumber uint64,
	genesis state.Genesis,
	reorgBlockNumChan chan struct{},
	cfg Config) (Synchronizer, error) {
	ctx, cancel := context.WithCancel(context.Background())

	return &ClientSynchronizer{
		state:             st,
		etherMan:          ethMan,
		ctx:               ctx,
		cancelCtx:         cancel,
		genBlockNumber:    genBlockNumber,
		genesis:           genesis,
		reorgBlockNumChan: reorgBlockNumChan,
		cfg:               cfg,
	}, nil
}

var waitDuration = time.Duration(0)

// Sync function will read the last state synced and will continue from that point.
// Sync() will read blockchain events to detect rollup updates
func (s *ClientSynchronizer) Sync() error {
	// If there is no lastEthereumBlock means that sync from the beginning is necessary. If not, it continues from the retrieved ethereum block
	// Get the latest synced block. If there is no block on db, use genesis block
	log.Info("Sync started")
	dbTx, err := s.state.BeginStateTransaction(s.ctx)
	if err != nil {
		log.Fatalf("error creating db transaction to get latest block")
	}
	lastEthBlockSynced, err := s.state.GetLastBlock(s.ctx, dbTx)
	if err != nil {
		if errors.Is(err, state.ErrStateNotSynchronized) {
			log.Info("State is empty, setting genesis block")
			header, err := s.etherMan.HeaderByNumber(s.ctx, big.NewInt(0).SetUint64(s.genBlockNumber))
			if err != nil {
				log.Fatal("error getting l1 block header for block ", s.genBlockNumber, " : ", err)
			}
			lastEthBlockSynced = &state.Block{
				BlockNumber: header.Number.Uint64(),
				BlockHash:   header.Hash(),
				ParentHash:  header.ParentHash,
				ReceivedAt:  time.Unix(int64(header.Time), 0),
			}
			if err := s.state.SetGenesis(s.ctx, *lastEthBlockSynced, s.genesis, dbTx); err != nil {
				log.Fatal("error setting genesis: ", err)
			}
		} else {
			log.Fatal("unexpected error getting the latest ethereum block. Error: ", err)
		}
	}
	err = dbTx.Commit(s.ctx)
	if err != nil {
		log.Errorf("error committing dbTx, err: %s", err.Error())
		rollbackErr := dbTx.Rollback(s.ctx)
		if rollbackErr != nil {
			log.Fatalf("error rolling back state. RollbackErr: %s, err: %s",
				rollbackErr.Error(), err.Error())
		}
		log.Fatalf("error committing dbTx, err: %s", err.Error())
	}
	for {
		select {
		case <-s.ctx.Done():
			return nil
		case <-time.After(waitDuration):
			//Sync L1Blocks
			if lastEthBlockSynced, err = s.syncBlocks(lastEthBlockSynced); err != nil {
				log.Warn("error syncing blocks: ", err)
				if s.ctx.Err() != nil {
					continue
				}
			}
			if waitDuration != s.cfg.SyncInterval.Duration {
				latestsequencedBatchNumber, err := s.etherMan.GetLatestBatchNumber()
				if err != nil {
					log.Warn("error getting latest sequenced batch in the rollup. Error: ", err)
					continue
				}
				// Check latest Synced Batch
				dbTx, err := s.state.BeginStateTransaction(s.ctx)
				if err != nil {
					log.Fatalf("error creating db transaction to get latestSyncedBatch. error: %s", err.Error())
				}
				latestSyncedBatch, err := s.state.GetLastBatchNumber(s.ctx, dbTx)
				errC := dbTx.Commit(s.ctx)
				if errC != nil {
					log.Errorf("error committing dbTx, err: %s", errC.Error())
					rollbackErr := dbTx.Rollback(s.ctx)
					if rollbackErr != nil {
						log.Fatalf("error rolling back state. RollbackErr: %s, err: %s",
							rollbackErr.Error(), errC.Error())
					}
					log.Fatalf("error committing dbTx, err: %s", errC.Error())
				}
				if err != nil {
					log.Warn("error getting latest batch synced. Error: ", err)
					continue
				}
				if latestSyncedBatch == latestsequencedBatchNumber {
					waitDuration = s.cfg.SyncInterval.Duration
				}
				if latestSyncedBatch > latestsequencedBatchNumber {
					log.Fatal("error: latest Synced BatchNumber is higher than the latest Proposed BatchNumber in the rollup")
				}
			}
			// Sync L2Blocks
			// TODO
		}
	}
}

// This function syncs the node from a specific block to the latest
func (s *ClientSynchronizer) syncBlocks(lastEthBlockSynced *state.Block) (*state.Block, error) {
	// This function will read events fromBlockNum to latestEthBlock. Check reorg to be sure that everything is ok.
	block, err := s.checkReorg(lastEthBlockSynced)
	if err != nil {
		log.Errorf("error checking reorgs. Retrying... Err: %s", err.Error())
		return lastEthBlockSynced, fmt.Errorf("error checking reorgs")
	}
	if block != nil {
		err = s.resetState(block.BlockNumber)
		if err != nil {
			log.Errorf("error resetting the state to a previous block. Err: %s, Retrying...", err.Error())
			return lastEthBlockSynced, fmt.Errorf("error resetting the state to a previous block")
		}
		return block, nil
	}

	// Call the blockchain to retrieve data
	header, err := s.etherMan.HeaderByNumber(s.ctx, nil)
	if err != nil {
		return lastEthBlockSynced, err
	}
	lastKnownBlock := header.Number

	var fromBlock uint64
	if lastEthBlockSynced.BlockNumber > 0 {
		fromBlock = lastEthBlockSynced.BlockNumber + 1
	}

	for {
		toBlock := fromBlock + s.cfg.SyncChunkSize

		log.Infof("Getting rollup info from block %d to block %d", fromBlock, toBlock)
		// This function returns the rollup information contained in the ethereum blocks and an extra param called order.
		// Order param is a map that contains the event order to allow the synchronizer store the info in the same order that is readed.
		// Name can be defferent in the order struct. For instance: Batches or Name:NewSequencers. This name is an identifier to check
		// if the next info that must be stored in the db is a new sequencer or a batch. The value pos (position) tells what is the
		// array index where this value is.
		blocks, order, err := s.etherMan.GetRollupInfoByBlockRange(s.ctx, fromBlock, &toBlock)
		if err != nil {
			return lastEthBlockSynced, err
		}
		s.processBlockRange(blocks, order)
		if len(blocks) > 0 {
			lastEthBlockSynced = &state.Block{
				BlockNumber: blocks[len(blocks)-1].BlockNumber,
				BlockHash:   blocks[len(blocks)-1].BlockHash,
				ParentHash:  blocks[len(blocks)-1].ParentHash,
				ReceivedAt:  blocks[len(blocks)-1].ReceivedAt,
			}
			for i := range blocks {
				log.Debug("Position: ", i, ". BlockNumber: ", blocks[i].BlockNumber, ". BlockHash: ", blocks[i].BlockHash)
			}
		}
		fromBlock = toBlock + 1

		if lastKnownBlock.Cmp(new(big.Int).SetUint64(toBlock)) < 1 {
			waitDuration = s.cfg.SyncInterval.Duration
			break
		}
		if len(blocks) == 0 { // If there is no events in the checked blocks range and lastKnownBlock > fromBlock.
			// Store the latest block of the block range. Get block info and process the block
			fb, err := s.etherMan.EthBlockByNumber(s.ctx, toBlock)
			if err != nil {
				return lastEthBlockSynced, err
			}
			b := etherman.Block{
				BlockNumber: fb.NumberU64(),
				BlockHash:   fb.Hash(),
				ParentHash:  fb.ParentHash(),
				ReceivedAt:  time.Unix(int64(fb.Time()), 0),
			}
			s.processBlockRange([]etherman.Block{b}, order)
			block := state.Block{
				BlockNumber: fb.NumberU64(),
				BlockHash:   fb.Hash(),
				ParentHash:  fb.ParentHash(),
				ReceivedAt:  time.Unix(int64(fb.Time()), 0),
			}
			lastEthBlockSynced = &block
			log.Debug("Storing empty block. BlockNumber: ", b.BlockNumber, ". BlockHash: ", b.BlockHash)
		}
	}

	return lastEthBlockSynced, nil
}

func (s *ClientSynchronizer) processBlockRange(blocks []etherman.Block, order map[common.Hash][]etherman.Order) {
	// New info has to be included into the db using the state
	for i := range blocks {
		// Begin db transaction
		dbTx, err := s.state.BeginStateTransaction(s.ctx)
		if err != nil {
			log.Fatalf("error creating db transaction to store block. BlockNumber: %d, error: %s", blocks[i].BlockNumber, err.Error())
		}
		b := state.Block{
			BlockNumber: blocks[i].BlockNumber,
			BlockHash:   blocks[i].BlockHash,
			ParentHash:  blocks[i].ParentHash,
			ReceivedAt:  blocks[i].ReceivedAt,
		}
		// Add block information
		err = s.state.AddBlock(s.ctx, &b, dbTx)
		if err != nil {
			rollbackErr := dbTx.Rollback(s.ctx)
			if rollbackErr != nil {
				log.Fatalf("error rolling back state to store block. BlockNumber: %d, rollbackErr: %s, error : %s", blocks[i].BlockNumber, rollbackErr.Error(), err.Error())
			}
			log.Fatalf("error storing block. BlockNumber: %d, error: %s", blocks[i].BlockNumber, err.Error())
		}
		for _, element := range order[blocks[i].BlockHash] {
			switch element.Name {
			case etherman.SequenceBatchesOrder:
				s.processSequenceBatches(blocks[i].SequencedBatches[element.Pos], blocks[i].BlockNumber, dbTx)
			case etherman.ForcedBatchesOrder:
				s.processForcedBatch(blocks[i].ForcedBatches[element.Pos], dbTx)
			case etherman.GlobalExitRootsOrder:
				s.processGlobalExitRoot(blocks[i].GlobalExitRoots[element.Pos], dbTx)
			case etherman.SequenceForceBatchesOrder:
				s.processSequenceForceBatch(blocks[i].SequencedForceBatches[element.Pos], blocks[i].BlockNumber, dbTx)
			case etherman.VerifyBatchOrder:
				s.processVerifiedBatch(blocks[i].VerifiedBatches[element.Pos], dbTx)
			}
		}
		err = dbTx.Commit(s.ctx)
		if err != nil {
			log.Errorf("error committing state to store block. BlockNumber: %d, err: %s", blocks[i].BlockNumber, err.Error())
			rollbackErr := dbTx.Rollback(s.ctx)
			if rollbackErr != nil {
				log.Fatalf("error rolling back state to store block. BlockNumber: %d, rollbackErr: %s, error : %s", blocks[i].BlockNumber, rollbackErr.Error(), err.Error())
			}
			log.Fatalf("error committing state to store block. BlockNumber: %d, err: %s", blocks[i].BlockNumber, err.Error())
		}
	}
}

// This function allows reset the state until an specific ethereum block
func (s *ClientSynchronizer) resetState(blockNumber uint64) error {
	log.Debug("Reverting synchronization to block: ", blockNumber)
	dbTx, err := s.state.BeginStateTransaction(s.ctx)
	if err != nil {
		log.Error("error starting a db transaction to reset the state. Error: ", err)
		return err
	}
	err = s.state.Reset(s.ctx, blockNumber, dbTx)
	if err != nil {
		rollbackErr := dbTx.Rollback(s.ctx)
		if rollbackErr != nil {
			log.Errorf("error rolling back state to store block. BlockNumber: %d, rollbackErr: %s, error : %s", blockNumber, rollbackErr.Error(), err.Error())
			return rollbackErr
		}
		log.Error("error resetting the state. Error: ", err)
		return err
	}
	err = dbTx.Commit(s.ctx)
	if err != nil {
		rollbackErr := dbTx.Rollback(s.ctx)
		if rollbackErr != nil {
			log.Errorf("error rolling back state to store block. BlockNumber: %d, rollbackErr: %s, error : %s", blockNumber, rollbackErr.Error(), err.Error())
			return rollbackErr
		}
		log.Error("error committing the resetted state. Error: ", err)
		return err
	}

	return nil
}

/*
This function will check if there is a reorg.
As input param needs the last ethereum block synced. Retrieve the block info from the blockchain
to compare it with the stored info. If hash and hash parent matches, then no reorg is detected and return a nil.
If hash or hash parent don't match, reorg detected and the function will return the block until the sync process
must be reverted. Then, check the previous ethereum block synced, get block info from the blockchain and check
hash and has parent. This operation has to be done until a match is found.
*/
func (s *ClientSynchronizer) checkReorg(latestBlock *state.Block) (*state.Block, error) {
	// This function only needs to worry about reorgs if some of the reorganized blocks contained rollup info.
	latestEthBlockSynced := *latestBlock
	var depth uint64
	for {
		block, err := s.etherMan.EthBlockByNumber(s.ctx, latestBlock.BlockNumber)
		if err != nil {
			log.Errorf("error getting latest block synced from blockchain. Block: %d, error: %s", latestBlock.BlockNumber, err.Error())
			return nil, err
		}
		if block.NumberU64() != latestBlock.BlockNumber {
			err = fmt.Errorf("Wrong ethereum block retrieved from blockchain. Block numbers don't match. BlockNumber stored: %d. BlockNumber retrieved: %d",
				latestBlock.BlockNumber, block.NumberU64())
			log.Error("error: ", err)
			return nil, err
		}
		// Compare hashes
		if (block.Hash() != latestBlock.BlockHash || block.ParentHash() != latestBlock.ParentHash) && latestBlock.BlockNumber > s.genBlockNumber {
			log.Debug("[checkReorg function] => latestBlockNumber: ", latestBlock.BlockNumber)
			log.Debug("[checkReorg function] => latestBlockHash: ", latestBlock.BlockHash)
			log.Debug("[checkReorg function] => latestBlockHashParent: ", latestBlock.ParentHash)
			log.Debug("[checkReorg function] => BlockNumber: ", latestBlock.BlockNumber, block.NumberU64())
			log.Debug("[checkReorg function] => BlockHash: ", block.Hash())
			log.Debug("[checkReorg function] => BlockHashParent: ", block.ParentHash())
			depth++
			log.Debug("REORG: Looking for the latest correct ethereum block. Depth: ", depth)
			// Reorg detected. Getting previous block
			dbTx, err := s.state.BeginStateTransaction(s.ctx)
			if err != nil {
				log.Fatalf("error creating db transaction to get prevoius blocks")
			}
			latestBlock, err = s.state.GetPreviousBlock(s.ctx, depth, dbTx)
			errC := dbTx.Commit(s.ctx)
			if errC != nil {
				log.Errorf("error committing dbTx, err: %s", errC.Error())
				rollbackErr := dbTx.Rollback(s.ctx)
				if rollbackErr != nil {
					log.Fatalf("error rolling back state. RollbackErr: %s, err: %s",
						rollbackErr.Error(), errC.Error())
				}
				log.Fatalf("error committing dbTx, err: %s", errC.Error())
			}
			if errors.Is(err, state.ErrNotFound) {
				log.Warn("error checking reorg: previous block not found in db: ", err)
				return &state.Block{}, nil
			} else if err != nil {
				return nil, err
			}
		} else {
			break
		}
	}
	if latestEthBlockSynced.BlockHash != latestBlock.BlockHash {
		log.Debug("Reorg detected in block: ", latestEthBlockSynced.BlockNumber)
		return latestBlock, nil
	}
	return nil, nil
}

// Stop function stops the synchronizer
func (s *ClientSynchronizer) Stop() {
	s.cancelCtx()
}

func (s *ClientSynchronizer) checkTrustedState(batch state.Batch, dbTx pgx.Tx) (bool, error) {
	// First get trusted batch from db
	tBatch, err := s.state.GetBatchByNumber(s.ctx, batch.BatchNumber, dbTx)
	if err != nil {
		return false, err
	}
	//Compare virtual state with trusted state
	if hex.EncodeToString(batch.BatchL2Data) == hex.EncodeToString(tBatch.BatchL2Data) &&
		batch.GlobalExitRoot.String() == tBatch.GlobalExitRoot.String() &&
		batch.Timestamp.Unix() == tBatch.Timestamp.Unix() &&
		batch.Coinbase.String() == tBatch.Coinbase.String() {
		return true, nil
	}
	return false, nil
}

func (s *ClientSynchronizer) processSequenceBatches(sequencedBatches []etherman.SequencedBatch, blockNumber uint64, dbTx pgx.Tx) {
	for _, sbatch := range sequencedBatches {
		vb := state.VirtualBatch{
			BatchNumber: sbatch.BatchNumber,
			TxHash:      sbatch.TxHash,
			Coinbase:    sbatch.Coinbase,
			BlockNumber: blockNumber,
		}
		virtualBatches := []state.VirtualBatch{vb}
		b := state.Batch{
			BatchNumber:    sbatch.BatchNumber,
			GlobalExitRoot: sbatch.GlobalExitRoot,
			Timestamp:      time.Unix(int64(sbatch.Timestamp), 0),
			Coinbase:       sbatch.Coinbase,
			BatchL2Data:    sbatch.Transactions,
		}
		batches := []state.Batch{b}
		// ForcedBatches must be processed after the batch.
		numForcedBatches := len(sbatch.ForceBatchesTimestamp)
		if numForcedBatches > 0 {
			// Read forcedBatches from db
			forcedBatches, err := s.state.GetNextForcedBatches(s.ctx, numForcedBatches, dbTx)
			if err != nil {
				log.Errorf("error getting forcedBatches. BatchNumber: %d", vb.BatchNumber)
				rollbackErr := dbTx.Rollback(s.ctx)
				if rollbackErr != nil {
					log.Fatalf("error rolling back state. BatchNumber: %d, BlockNumber: %d, rollbackErr: %s, error : %s", vb.BatchNumber, blockNumber, rollbackErr.Error(), err.Error())
				}
				log.Fatalf("error getting forcedBatches. BatchNumber: %d, BlockNumber: %d, error: %s", vb.BatchNumber, blockNumber, err.Error())
			}
			if numForcedBatches != len(forcedBatches) {
				rollbackErr := dbTx.Rollback(s.ctx)
				if rollbackErr != nil {
					log.Fatalf("error rolling back state. BatchNumber: %d, BlockNumber: %d, rollbackErr: %s", vb.BatchNumber, blockNumber, rollbackErr.Error())
				}
				log.Fatal("error number of forced batches doesn't match")
			}
			for i, forcedBatch := range forcedBatches {
				vb := state.VirtualBatch{
					BatchNumber: sbatch.BatchNumber + uint64(i),
					TxHash:      sbatch.TxHash,
					Coinbase:    sbatch.Coinbase,
					BlockNumber: blockNumber,
				}
				virtualBatches = append(virtualBatches, vb)
				tb := state.Batch{
					BatchNumber:    sbatch.BatchNumber + uint64(i), // First process the batch and then the forcedBatches
					GlobalExitRoot: forcedBatch.GlobalExitRoot,
					Timestamp:      time.Unix(int64(sbatch.ForceBatchesTimestamp[i]), 0), // ForceBatchesTimestamp instead of forcedAt because it is the timestamp selected by the sequencer, not when the forced batch was sent. This forcedAt is the min timestamp allowed.
					Coinbase:       forcedBatch.Sequencer,
					BatchL2Data:    forcedBatch.RawTxsData,
				}
				batches = append(batches, tb)
				// Store batchNumber in forced_batch table
				err = s.state.AddBatchNumberInForcedBatch(s.ctx, forcedBatch.ForcedBatchNumber, tb.BatchNumber, dbTx)
				if err != nil {
					log.Errorf("error adding the batchNumber to forcedBatch in processSequenceBatches. BlockNumber: %d", blockNumber)
					rollbackErr := dbTx.Rollback(s.ctx)
					if rollbackErr != nil {
						log.Fatalf("error rolling back state. BlockNumber: %d, rollbackErr: %s, error : %s", blockNumber, rollbackErr.Error(), err.Error())
					}
					log.Fatalf("error adding the batchNumber to forcedBatch in processSequenceBatches. BlockNumber: %d, error: %s", blockNumber, err.Error())
				}
			}
		}

		if len(virtualBatches) != len(batches) {
			rollbackErr := dbTx.Rollback(s.ctx)
			if rollbackErr != nil {
				log.Fatalf("error rolling back state. BatchNumber: %d, BlockNumber: %d, rollbackErr: %s", vb.BatchNumber, blockNumber, rollbackErr.Error())
			}
			log.Fatal("error: length of batches and virtualBatches don't match.\nvirtualBatches: %+v \nbatches: %+v", virtualBatches, batches)
		}

		// Now we need to check all the batches. ForcedBatches should be already stored in the batch table because this is done by the sequencer
		for i, batch := range batches {
			processCtx := state.ProcessingContext{
				BatchNumber:    batch.BatchNumber,
				Coinbase:       batch.Coinbase,
				Timestamp:      batch.Timestamp,
				GlobalExitRoot: batch.GlobalExitRoot,
			}
			// Call the check trusted state method to compare trusted and virtual state
			status, err := s.checkTrustedState(batch, dbTx)
			if err != nil {
				if errors.Is(err, state.ErrNotFound) {
					log.Debugf("BatchNumber: %d, not found in trusted state. Storing it...", batch.BatchNumber)
					// If it is not found, store batch
					err = s.state.ProcessAndStoreClosedBatch(s.ctx, processCtx, batch.BatchL2Data, dbTx)
					if err != nil {
						log.Errorf("error storing trustedBatch. BatchNumber: %d, BlockNumber: %d, error: %s", batch.BatchNumber, blockNumber, err.Error())
						rollbackErr := dbTx.Rollback(s.ctx)
						if rollbackErr != nil {
							log.Fatalf("error rolling back state. BatchNumber: %d, BlockNumber: %d, rollbackErr: %s, error : %s", batch.BatchNumber, blockNumber, rollbackErr.Error(), err.Error())
						}
						log.Fatalf("error storing batch. BatchNumber: %d, BlockNumber: %d, error: %s", batch.BatchNumber, blockNumber, err.Error())
					}
					status = true
				} else {
					rollbackErr := dbTx.Rollback(s.ctx)
					if rollbackErr != nil {
						log.Fatalf("error rolling back state. BatchNumber: %d, BlockNumber: %d, rollbackErr: %s, error : %s", vb.BatchNumber, blockNumber, rollbackErr.Error(), err.Error())
					}
					log.Fatal("error checking trusted state: ", err)
				}
			}
			if !status {
				// Reset trusted state
				log.Infof("reorg detected, discarding batches until batchNum %d", batch.BatchNumber)
				err := s.state.ResetTrustedState(s.ctx, batch.BatchNumber, dbTx) // This method has to reset the forced batches deleting the batchNumber for higher batchNumbers
				if err != nil {
					log.Errorf("error resetting trusted state. BatchNumber: %d, BlockNumber: %d, error: %s", batch.BatchNumber, blockNumber, err.Error())
					rollbackErr := dbTx.Rollback(s.ctx)
					if rollbackErr != nil {
						log.Fatalf("error rolling back state. BatchNumber: %d, BlockNumber: %d, rollbackErr: %s, error : %s", batch.BatchNumber, blockNumber, rollbackErr.Error(), err.Error())
					}
					log.Fatalf("error resetting trusted state. BatchNumber: %d, BlockNumber: %d, error: %s", batch.BatchNumber, blockNumber, err.Error())
				}
				err = s.state.ProcessAndStoreClosedBatch(s.ctx, processCtx, batch.BatchL2Data, dbTx)
				if err != nil {
					log.Errorf("error storing trustedBatch. BatchNumber: %d, BlockNumber: %d, error: %s", batch.BatchNumber, blockNumber, err.Error())
					rollbackErr := dbTx.Rollback(s.ctx)
					if rollbackErr != nil {
						log.Fatalf("error rolling back state. BatchNumber: %d, BlockNumber: %d, rollbackErr: %s, error : %s", batch.BatchNumber, blockNumber, rollbackErr.Error(), err.Error())
					}
					log.Fatalf("error storing batch. BatchNumber: %d, BlockNumber: %d, error: %s", batch.BatchNumber, blockNumber, err.Error())
				}
			}
			// Store virtualBatch
			err = s.state.AddVirtualBatch(s.ctx, &virtualBatches[i], dbTx)
			if err != nil {
				log.Errorf("error storing virtualBatch. BatchNumber: %d, BlockNumber: %d, error: %s", virtualBatches[i].BatchNumber, blockNumber, err.Error())
				rollbackErr := dbTx.Rollback(s.ctx)
				if rollbackErr != nil {
					log.Fatalf("error rolling back state. BatchNumber: %d, BlockNumber: %d, rollbackErr: %s, error : %s", virtualBatches[i].BatchNumber, blockNumber, rollbackErr.Error(), err.Error())
				}
				log.Fatalf("error storing virtualBatch. BatchNumber: %d, BlockNumber: %d, error: %s", virtualBatches[i].BatchNumber, blockNumber, err.Error())
			}
		}
	}
}

func (s *ClientSynchronizer) processSequenceForceBatch(sequenceForceBatch etherman.SequencedForceBatch, blockNumber uint64, dbTx pgx.Tx) {
	// First, reset trusted state
	lastVirtualizedBatchNumber := sequenceForceBatch.LastBatchSequenced - sequenceForceBatch.ForceBatchNumber
	err := s.state.ResetTrustedState(s.ctx, lastVirtualizedBatchNumber, dbTx) // This method has to reset the forced batches deleting the batchNumber for higher batchNumbers
	if err != nil {
		log.Errorf("error resetting trusted state. BatchNumber: %d, BlockNumber: %d, error: %s", lastVirtualizedBatchNumber, blockNumber, err.Error())
		rollbackErr := dbTx.Rollback(s.ctx)
		if rollbackErr != nil {
			log.Fatalf("error rolling back state. BatchNumber: %d, BlockNumber: %d, rollbackErr: %s, error : %s", lastVirtualizedBatchNumber, blockNumber, rollbackErr.Error(), err.Error())
		}
		log.Fatalf("error resetting trusted state. BatchNumber: %d, BlockNumber: %d, error: %s", lastVirtualizedBatchNumber, blockNumber, err.Error())
	}
	// Read forcedBatches from db
	forcedBatches, err := s.state.GetNextForcedBatches(s.ctx, int(sequenceForceBatch.ForceBatchNumber), dbTx)
	if err != nil {
		log.Errorf("error getting forcedBatches in processSequenceForceBatch. BlockNumber: %d", blockNumber)
		rollbackErr := dbTx.Rollback(s.ctx)
		if rollbackErr != nil {
			log.Fatalf("error rolling back state. BlockNumber: %d, rollbackErr: %s, error : %s", blockNumber, rollbackErr.Error(), err.Error())
		}
		log.Fatalf("error getting forcedBatches in processSequenceForceBatch. BlockNumber: %d, error: %s", blockNumber, err.Error())
	}
	if int(sequenceForceBatch.ForceBatchNumber) != len(forcedBatches) {
		rollbackErr := dbTx.Rollback(s.ctx)
		if rollbackErr != nil {
			log.Fatalf("error rolling back state. BlockNumber: %d, rollbackErr: %s, error : %s", blockNumber, rollbackErr.Error(), err.Error())
		}
		log.Fatal("error number of forced batches doesn't match")
	}

	for i, fbatch := range forcedBatches {
		vb := state.VirtualBatch{
			BatchNumber: sequenceForceBatch.LastBatchSequenced - sequenceForceBatch.ForceBatchNumber + uint64(i),
			TxHash:      sequenceForceBatch.TxHash,
			Coinbase:    sequenceForceBatch.Coinbase,
			BlockNumber: blockNumber,
		}
		b := state.ProcessingContext{
			BatchNumber:    sequenceForceBatch.LastBatchSequenced - sequenceForceBatch.ForceBatchNumber + uint64(i),
			GlobalExitRoot: fbatch.GlobalExitRoot,
			Timestamp:      fbatch.ForcedAt,
			Coinbase:       fbatch.Sequencer,
		}
		// Process batch
		err := s.state.ProcessAndStoreClosedBatch(s.ctx, b, fbatch.RawTxsData, dbTx)
		if err != nil {
			log.Errorf("error processing batch in processSequenceForceBatch. BatchNumber: %d, BlockNumber: %d, error: %s", b.BatchNumber, blockNumber, err.Error())
			rollbackErr := dbTx.Rollback(s.ctx)
			if rollbackErr != nil {
				log.Fatalf("error rolling back state. BatchNumber: %d, BlockNumber: %d, rollbackErr: %s, error : %s", b.BatchNumber, blockNumber, rollbackErr.Error(), err.Error())
			}
			log.Fatalf("error processing batch in processSequenceForceBatch. BatchNumber: %d, BlockNumber: %d, error: %s", b.BatchNumber, blockNumber, err.Error())
		}
		// Store virtualBatch
		err = s.state.AddVirtualBatch(s.ctx, &vb, dbTx)
		if err != nil {
			log.Errorf("error storing virtualBatch in processSequenceForceBatch. BatchNumber: %d, BlockNumber: %d, error: %s", vb.BatchNumber, blockNumber, err.Error())
			rollbackErr := dbTx.Rollback(s.ctx)
			if rollbackErr != nil {
				log.Fatalf("error rolling back state. BatchNumber: %d, BlockNumber: %d, rollbackErr: %s, error : %s", vb.BatchNumber, blockNumber, rollbackErr.Error(), err.Error())
			}
			log.Fatalf("error storing virtualBatch in processSequenceForceBatch. BatchNumber: %d, BlockNumber: %d, error: %s", vb.BatchNumber, blockNumber, err.Error())
		}
		// Store batchNumber in forced_batch table
		err = s.state.AddBatchNumberInForcedBatch(s.ctx, fbatch.ForcedBatchNumber, vb.BatchNumber, dbTx)
		if err != nil {
			log.Errorf("error adding the batchNumber to forcedBatch in processSequenceForceBatch. BlockNumber: %d", blockNumber)
			rollbackErr := dbTx.Rollback(s.ctx)
			if rollbackErr != nil {
				log.Fatalf("error rolling back state. BlockNumber: %d, rollbackErr: %s, error : %s", blockNumber, rollbackErr.Error(), err.Error())
			}
			log.Fatalf("error adding the batchNumber to forcedBatch in processSequenceForceBatch. BlockNumber: %d, error: %s", blockNumber, err.Error())
		}
	}
}

func (s *ClientSynchronizer) processForcedBatch(forcedBatch etherman.ForcedBatch, dbTx pgx.Tx) {
	// Store forced batch into the db
	forcedB := state.ForcedBatch{
		BlockNumber:       forcedBatch.BlockNumber,
		BatchNumber:       nil,
		ForcedBatchNumber: forcedBatch.ForcedBatchNumber,
		Sequencer:         forcedBatch.Sequencer,
		GlobalExitRoot:    forcedBatch.GlobalExitRoot,
		RawTxsData:        forcedBatch.RawTxsData,
		ForcedAt:          forcedBatch.ForcedAt,
	}
	err := s.state.AddForcedBatch(s.ctx, &forcedB, dbTx)
	if err != nil {
		log.Errorf("error storing the forcedBatch in processForcedBatch. BlockNumber: %d", forcedBatch.BlockNumber)
		rollbackErr := dbTx.Rollback(s.ctx)
		if rollbackErr != nil {
			log.Fatalf("error rolling back state. BlockNumber: %d, rollbackErr: %s, error : %s", forcedBatch.BlockNumber, rollbackErr.Error(), err.Error())
		}
		log.Fatalf("error storing the forcedBatch in processForcedBatch. BlockNumber: %d, error: %s", forcedBatch.BlockNumber, err.Error())
	}
}

func (s *ClientSynchronizer) processGlobalExitRoot(globalExitRoot etherman.GlobalExitRoot, dbTx pgx.Tx) {
	// Store GlobalExitRoot
	ger := state.GlobalExitRoot{
		BlockNumber:       globalExitRoot.BlockNumber,
		GlobalExitRootNum: globalExitRoot.GlobalExitRootNum,
		MainnetExitRoot:   globalExitRoot.MainnetExitRoot,
		RollupExitRoot:    globalExitRoot.RollupExitRoot,
		GlobalExitRoot:    globalExitRoot.GlobalExitRoot,
	}
	err := s.state.AddGlobalExitRoot(s.ctx, &ger, dbTx)
	if err != nil {
		log.Errorf("error storing the GlobalExitRoot in processGlobalExitRoot. BlockNumber: %d", globalExitRoot.BlockNumber)
		rollbackErr := dbTx.Rollback(s.ctx)
		if rollbackErr != nil {
			log.Fatalf("error rolling back state. BlockNumber: %d, rollbackErr: %s, error : %s", globalExitRoot.BlockNumber, rollbackErr.Error(), err.Error())
		}
		log.Fatalf("error storing the GlobalExitRoot in processGlobalExitRoot. BlockNumber: %d, error: %s", globalExitRoot.BlockNumber, err.Error())
	}
}

func (s *ClientSynchronizer) processVerifiedBatch(verifiedBatch etherman.VerifiedBatch, dbTx pgx.Tx) {
	verifiedB := state.VerifiedBatch{
		BlockNumber: verifiedBatch.BlockNumber,
		BatchNumber: verifiedBatch.BatchNumber,
		Aggregator:  verifiedBatch.Aggregator,
		TxHash:      verifiedBatch.TxHash,
	}
	err := s.state.AddVerifiedBatch(s.ctx, &verifiedB, dbTx)
	if err != nil {
		log.Errorf("error storing the verifiedBatch in processVerifiedBatch. BlockNumber: %d", verifiedBatch.BlockNumber)
		rollbackErr := dbTx.Rollback(s.ctx)
		if rollbackErr != nil {
			log.Fatalf("error rolling back state. BlockNumber: %d, rollbackErr: %s, error : %s", verifiedBatch.BlockNumber, rollbackErr.Error(), err.Error())
		}
		log.Fatalf("error storing the verifiedBatch in processVerifiedBatch. BlockNumber: %d, error: %s", verifiedBatch.BlockNumber, err.Error())
	}
}
