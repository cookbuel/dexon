// Copyright 2018 The dexon-consensus-core Authors
// This file is part of the dexon-consensus-core library.
//
// The dexon-consensus-core library is free software: you can redistribute it
// and/or modify it under the terms of the GNU Lesser General Public License as
// published by the Free Software Foundation, either version 3 of the License,
// or (at your option) any later version.
//
// The dexon-consensus-core library is distributed in the hope that it will be
// useful, but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU Lesser
// General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the dexon-consensus-core library. If not, see
// <http://www.gnu.org/licenses/>.

package core

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/dexon-foundation/dexon-consensus-core/common"
	"github.com/dexon-foundation/dexon-consensus-core/core/blockdb"
	"github.com/dexon-foundation/dexon-consensus-core/core/types"
)

// Errors for sanity check error.
var (
	ErrAckingBlockNotExists    = fmt.Errorf("acking block not exists")
	ErrDuplicatedAckOnOneChain = fmt.Errorf("duplicated ack on one chain")
	ErrInvalidChainID          = fmt.Errorf("invalid chain id")
	ErrInvalidProposerID       = fmt.Errorf("invalid proposer id")
	ErrInvalidWitness          = fmt.Errorf("invalid witness data")
	ErrInvalidBlock            = fmt.Errorf("invalid block")
	ErrNotAckParent            = fmt.Errorf("not ack parent")
	ErrDoubleAck               = fmt.Errorf("double ack")
	ErrAcksNotSorted           = fmt.Errorf("acks not sorted")
	ErrInvalidBlockHeight      = fmt.Errorf("invalid block height")
	ErrAlreadyInLattice        = fmt.Errorf("block already in lattice")
	ErrIncorrectBlockTime      = fmt.Errorf("block timestamp is incorrect")
	ErrInvalidRoundID          = fmt.Errorf("invalid round id")
	ErrUnknownRoundID          = fmt.Errorf("unknown round id")
	ErrRoundOutOfRange         = fmt.Errorf("round out of range")
	ErrRoundNotSwitch          = fmt.Errorf("round not switch")
	ErrNotGenesisBlock         = fmt.Errorf("not a genesis block")
	ErrUnexpectedGenesisBlock  = fmt.Errorf("unexpected genesis block")
)

// Errors for method usage
var (
	ErrRoundNotIncreasing     = errors.New("round not increasing")
	ErrPurgedBlockNotFound    = errors.New("purged block not found")
	ErrPurgeNotDeliveredBlock = errors.New("not purge from head")
)

// latticeDataConfig is the configuration for latticeData for each round.
type latticeDataConfig struct {
	roundBasedConfig
	// Number of chains between runs
	numChains uint32
	// Block interval specifies reasonable time difference between
	// parent/child blocks.
	minBlockTimeInterval time.Duration
	maxBlockTimeInterval time.Duration
}

// Initiate latticeDataConfig from types.Config.
func (config *latticeDataConfig) fromConfig(roundID uint64, cfg *types.Config) {
	config.numChains = cfg.NumChains
	config.minBlockTimeInterval = cfg.MinBlockInterval
	config.maxBlockTimeInterval = cfg.MaxBlockInterval
	config.setupRoundBasedFields(roundID, cfg)
}

// Check if timestamp of a block is valid according to a reference time.
func (config *latticeDataConfig) isValidBlockTime(
	b *types.Block, ref time.Time) bool {
	return !(b.Timestamp.Before(ref.Add(config.minBlockTimeInterval)) ||
		b.Timestamp.After(ref.Add(config.maxBlockTimeInterval)))
}

// isValidGenesisBlockTime check if a timestamp is valid for a genesis block.
func (config *latticeDataConfig) isValidGenesisBlockTime(b *types.Block) bool {
	return !(b.Timestamp.Before(config.roundBeginTime) || b.Timestamp.After(
		config.roundBeginTime.Add(config.maxBlockTimeInterval)))
}

// newGenesisLatticeDataConfig constructs a latticeDataConfig instance.
func newGenesisLatticeDataConfig(
	dMoment time.Time, config *types.Config) *latticeDataConfig {
	c := &latticeDataConfig{}
	c.fromConfig(0, config)
	c.setRoundBeginTime(dMoment)
	return c
}

// newLatticeDataConfig constructs a latticeDataConfig instance.
func newLatticeDataConfig(
	prev *latticeDataConfig, cur *types.Config) *latticeDataConfig {
	c := &latticeDataConfig{}
	c.fromConfig(prev.roundID+1, cur)
	c.setRoundBeginTime(prev.roundEndTime)
	return c
}

// latticeData is a module for storing lattice.
type latticeData struct {
	// we need blockdb to read blocks purged from cache in memory.
	db blockdb.Reader
	// chains stores chains' blocks and other info.
	chains []*chainStatus
	// blockByHash stores blocks, indexed by block hash.
	blockByHash map[common.Hash]*types.Block
	// This stores configuration for each round.
	configs []*latticeDataConfig
}

// newLatticeData creates a new latticeData struct.
func newLatticeData(
	db blockdb.Reader, genesisConfig *latticeDataConfig) (data *latticeData) {
	data = &latticeData{
		db:          db,
		chains:      make([]*chainStatus, genesisConfig.numChains),
		blockByHash: make(map[common.Hash]*types.Block),
		configs:     []*latticeDataConfig{genesisConfig},
	}
	for i := range data.chains {
		data.chains[i] = &chainStatus{
			ID:         uint32(i),
			blocks:     []*types.Block{},
			lastAckPos: make([]*types.Position, genesisConfig.numChains),
		}
	}
	return
}

func (data *latticeData) checkAckingRelations(b *types.Block) error {
	acksByChainID := make(map[uint32]struct{}, len(data.chains))
	for _, hash := range b.Acks {
		bAck, err := data.findBlock(hash)
		if err != nil {
			if err == blockdb.ErrBlockDoesNotExist {
				return ErrAckingBlockNotExists
			}
			return err
		}
		// Check if it acks blocks from old rounds, the allowed round difference
		// is 1.
		if DiffUint64(bAck.Position.Round, b.Position.Round) > 1 {
			return ErrRoundOutOfRange
		}
		// Check if it acks older blocks than blocks on the same chain.
		lastAckPos :=
			data.chains[bAck.Position.ChainID].lastAckPos[b.Position.ChainID]
		if lastAckPos != nil && !bAck.Position.Newer(lastAckPos) {
			return ErrDoubleAck
		}
		// Check if ack two blocks on the same chain. This would need
		// to check after we replace map with slice for acks.
		if _, acked := acksByChainID[bAck.Position.ChainID]; acked {
			return ErrDuplicatedAckOnOneChain
		}
		acksByChainID[bAck.Position.ChainID] = struct{}{}
	}
	return nil
}

func (data *latticeData) sanityCheck(b *types.Block) error {
	// TODO(mission): Check if its proposer is in validator set somewhere,
	//                lattice doesn't have to know about node set.
	config := data.getConfig(b.Position.Round)
	if config == nil {
		return ErrInvalidRoundID
	}
	// Check if the chain id is valid.
	if b.Position.ChainID >= config.numChains {
		return ErrInvalidChainID
	}
	// Make sure parent block is arrived.
	chain := data.chains[b.Position.ChainID]
	chainTip := chain.tip
	if chainTip == nil {
		if !b.ParentHash.Equal(common.Hash{}) {
			return ErrAckingBlockNotExists
		}
		if !b.IsGenesis() {
			return ErrNotGenesisBlock
		}
		if !config.isValidGenesisBlockTime(b) {
			return ErrIncorrectBlockTime
		}
		return data.checkAckingRelations(b)
	}
	// Check parent block if parent hash is specified.
	if !b.ParentHash.Equal(common.Hash{}) {
		if !b.ParentHash.Equal(chainTip.Hash) {
			return ErrAckingBlockNotExists
		}
		if !b.IsAcking(b.ParentHash) {
			return ErrNotAckParent
		}
	}
	chainTipConfig := data.getConfig(chainTip.Position.Round)
	// Round can't be rewinded.
	if chainTip.Position.Round > b.Position.Round {
		return ErrInvalidRoundID
	}
	checkTip := false
	if chainTip.Timestamp.After(chainTipConfig.roundEndTime) {
		// Round switching should happen when chainTip already pass
		// round end time of its round.
		if chainTip.Position.Round == b.Position.Round {
			return ErrRoundNotSwitch
		}
		// The round ID is continuous.
		if b.Position.Round-chainTip.Position.Round == 1 {
			checkTip = true
		} else {
			// This block should be genesis block of new round because round
			// ID is not continuous.
			if !b.IsGenesis() {
				return ErrNotGenesisBlock
			}
			if !config.isValidGenesisBlockTime(b) {
				return ErrIncorrectBlockTime
			}
			// TODO(mission): make sure rounds between chainTip and current block
			//                don't expect blocks from this chain.
		}
	} else {
		if chainTip.Position.Round != b.Position.Round {
			// Round should not switch.
			return ErrInvalidRoundID
		}
		checkTip = true
	}
	// Validate the relation between chain tip when needed.
	if checkTip {
		if b.Position.Height != chainTip.Position.Height+1 {
			return ErrInvalidBlockHeight
		}
		if b.Witness.Height < chainTip.Witness.Height {
			return ErrInvalidWitness
		}
		if !config.isValidBlockTime(b, chainTip.Timestamp) {
			return ErrIncorrectBlockTime
		}
		// Chain tip should be acked.
		if !b.IsAcking(chainTip.Hash) {
			return ErrNotAckParent
		}
	}
	if err := data.checkAckingRelations(b); err != nil {
		return err
	}
	return nil
}

// addBlock processes block, it does sanity check, inserts block into
// lattice and deletes blocks which will not be used.
func (data *latticeData) addBlock(
	block *types.Block) (deliverable []*types.Block, err error) {
	var (
		bAck    *types.Block
		updated bool
	)
	// TODO(mission): sanity check twice, might hurt performance.
	// If a block does not pass sanity check, report error.
	if err = data.sanityCheck(block); err != nil {
		return
	}
	if err = data.chains[block.Position.ChainID].addBlock(block); err != nil {
		return
	}
	data.blockByHash[block.Hash] = block
	// Update lastAckPos.
	for _, ack := range block.Acks {
		if bAck, err = data.findBlock(ack); err != nil {
			return
		}
		data.chains[bAck.Position.ChainID].lastAckPos[block.Position.ChainID] =
			bAck.Position.Clone()
	}
	// Extract blocks that deliverable to total ordering.
	// A block is deliverable to total ordering iff:
	//  - All its acking blocks are delivered to total ordering.
	for {
		updated = false
		for _, status := range data.chains {
			if status.nextOutputIndex >= len(status.blocks) {
				continue
			}
			tip := status.blocks[status.nextOutputIndex]
			allAckingBlockDelivered := true
			for _, ack := range tip.Acks {
				if bAck, err = data.findBlock(ack); err != nil {
					return
				}
				// Check if this block is outputed or not.
				idx := data.chains[bAck.Position.ChainID].findBlock(
					&bAck.Position)
				if idx == -1 ||
					idx < data.chains[bAck.Position.ChainID].nextOutputIndex {
					continue
				}
				// This acked block exists and not delivered yet.
				allAckingBlockDelivered = false
			}
			if allAckingBlockDelivered {
				status.nextOutputIndex++
				deliverable = append(deliverable, tip)
				updated = true
			}
		}
		if !updated {
			break
		}
	}
	return
}

// prepareBlock helps to setup fields of block based on its ChainID and Round,
// including:
//  - Acks
//  - Timestamp
//  - ParentHash and Height from parent block. If there is no valid parent block
//    (ex. Newly added chain or bootstrap ), these fields would be setup as
//    genesis block.
func (data *latticeData) prepareBlock(b *types.Block) error {
	var (
		minTimestamp, maxTimestamp time.Time
		config                     *latticeDataConfig
		acks                       common.Hashes
		bindTip                    bool
		chainTip                   *types.Block
	)
	if config = data.getConfig(b.Position.Round); config == nil {
		return ErrUnknownRoundID
	}
	// Reset fields to make sure we got these information from parent block.
	b.Position.Height = 0
	b.ParentHash = common.Hash{}
	// Decide valid timestamp range.
	homeChain := data.chains[b.Position.ChainID]
	if homeChain.tip != nil {
		chainTip = homeChain.tip
		if b.Position.Round < chainTip.Position.Round {
			return ErrInvalidRoundID
		}
		chainTipConfig := data.getConfig(chainTip.Position.Round)
		if chainTip.Timestamp.After(chainTipConfig.roundEndTime) {
			if b.Position.Round == chainTip.Position.Round {
				return ErrRoundNotSwitch
			}
			if b.Position.Round == chainTip.Position.Round+1 {
				bindTip = true
			}
		} else {
			if b.Position.Round != chainTip.Position.Round {
				return ErrInvalidRoundID
			}
			bindTip = true
		}
		// TODO(mission): find a way to prevent us to assign a witness height
		//                from Jurassic period.
		b.Witness.Height = chainTip.Witness.Height
	}
	// For blocks with continuous round ID, assign timestamp range based on
	// parent block and bound config.
	if bindTip {
		minTimestamp = chainTip.Timestamp.Add(config.minBlockTimeInterval)
		maxTimestamp = chainTip.Timestamp.Add(config.maxBlockTimeInterval)
		// When a chain is removed and added back, the reference block
		// of previous round can't be used as parent block.
		b.ParentHash = chainTip.Hash
		b.Position.Height = chainTip.Position.Height + 1
	} else {
		// Discontinuous round ID detected, another fresh start of
		// new round.
		minTimestamp = config.roundBeginTime
		maxTimestamp = config.roundBeginTime.Add(config.maxBlockTimeInterval)
	}
	// Fix timestamp if the given one is invalid.
	if b.Timestamp.Before(minTimestamp) {
		b.Timestamp = minTimestamp
	} else if b.Timestamp.After(maxTimestamp) {
		b.Timestamp = maxTimestamp
	}
	// Setup acks fields.
	for _, status := range data.chains {
		// Check if we can ack latest block on that chain.
		if status.tip == nil {
			continue
		}
		lastAckPos := status.lastAckPos[b.Position.ChainID]
		if lastAckPos != nil && !status.tip.Position.Newer(lastAckPos) {
			// The reference block is already acked.
			continue
		}
		// Can't ack block too old or too new to us.
		if DiffUint64(
			status.tip.Position.Round, b.Position.Round) > 1 {
			continue
		}
		acks = append(acks, status.tip.Hash)
	}
	b.Acks = common.NewSortedHashes(acks)
	return nil
}

// prepareEmptyBlock helps to setup fields of block based on its ChainID.
// including:
//  - Acks only acking its parent
//  - Timestamp with parent.Timestamp + minBlockProposeInterval
//  - ParentHash and Height from parent block. If there is no valid parent block
//    (ex. Newly added chain or bootstrap ), these fields would be setup as
//    genesis block.
func (data *latticeData) prepareEmptyBlock(b *types.Block) {
	// emptyBlock has no proposer.
	b.ProposerID = types.NodeID{}
	var acks common.Hashes
	// Reset fields to make sure we got these information from parent block.
	b.Position.Height = 0
	b.Position.Round = 0
	b.ParentHash = common.Hash{}
	b.Timestamp = time.Time{}
	// Decide valid timestamp range.
	homeChain := data.chains[b.Position.ChainID]
	if homeChain.tip != nil {
		chainTip := homeChain.tip
		b.ParentHash = chainTip.Hash
		chainTipConfig := data.getConfig(chainTip.Position.Round)
		if chainTip.Timestamp.After(chainTipConfig.roundEndTime) {
			b.Position.Round = chainTip.Position.Round + 1
		} else {
			b.Position.Round = chainTip.Position.Round
		}
		b.Position.Height = chainTip.Position.Height + 1
		b.Timestamp = chainTip.Timestamp.Add(chainTipConfig.minBlockTimeInterval)
		acks = append(acks, chainTip.Hash)
	}
	b.Acks = common.NewSortedHashes(acks)
}

// TODO(mission): make more abstraction for this method.
// nextHeight returns the next height for the chain.
func (data *latticeData) nextPosition(chainID uint32) types.Position {
	return data.chains[chainID].nextPosition()
}

// findBlock seeks blocks in memory or db.
func (data *latticeData) findBlock(h common.Hash) (b *types.Block, err error) {
	if b = data.blockByHash[h]; b != nil {
		return
	}
	var tmpB types.Block
	if tmpB, err = data.db.Get(h); err != nil {
		return
	}
	b = &tmpB
	return
}

// purgeBlocks purges blocks from cache.
func (data *latticeData) purgeBlocks(blocks []*types.Block) error {
	for _, b := range blocks {
		if _, exists := data.blockByHash[b.Hash]; !exists {
			return ErrPurgedBlockNotFound
		}
		delete(data.blockByHash, b.Hash)
		// blocks would be purged in ascending order in position.
		if err := data.chains[b.Position.ChainID].purgeBlock(b); err != nil {
			return err
		}
	}
	return nil
}

// getConfig get configuration for lattice-data by round ID.
func (data *latticeData) getConfig(round uint64) (config *latticeDataConfig) {
	if round >= uint64(len(data.configs)) {
		return
	}
	return data.configs[round]
}

// appendConfig appends a configuration for upcoming round. When you append
// a config for round R, next time you can only append the config for round R+1.
func (data *latticeData) appendConfig(
	round uint64, config *types.Config) (err error) {
	// Make sure caller knows which round this config belongs to.
	if round != uint64(len(data.configs)) {
		return ErrRoundNotIncreasing
	}
	// Set round beginning time.
	newConfig := newLatticeDataConfig(data.configs[len(data.configs)-1], config)
	data.configs = append(data.configs, newConfig)
	// Resize each slice if incoming config contains larger number of chains.
	if uint32(len(data.chains)) < newConfig.numChains {
		count := newConfig.numChains - uint32(len(data.chains))
		for _, status := range data.chains {
			status.lastAckPos = append(
				status.lastAckPos, make([]*types.Position, count)...)
		}
		for i := uint32(len(data.chains)); i < newConfig.numChains; i++ {
			data.chains = append(data.chains, &chainStatus{
				ID:         i,
				blocks:     []*types.Block{},
				lastAckPos: make([]*types.Position, newConfig.numChains),
			})
		}
	}
	return nil
}

type chainStatus struct {
	// ID keeps the chainID of this chain status.
	ID uint32
	// blocks stores blocks proposed for this chain, sorted by height.
	blocks []*types.Block
	// tip is the last block on this chain.
	tip *types.Block
	// lastAckPos caches last acking position from other chains. Nil means
	// not acked yet.
	lastAckPos []*types.Position
	// the index to be output next time.
	nextOutputIndex int
}

// findBlock finds index of block in current pending blocks on this chain.
// -1 means not found.
func (s *chainStatus) findBlock(pos *types.Position) (idx int) {
	idx = sort.Search(len(s.blocks), func(i int) bool {
		return s.blocks[i].Position.Newer(pos) ||
			s.blocks[i].Position.Equal(pos)
	})
	if idx == len(s.blocks) {
		idx = -1
	} else if !s.blocks[idx].Position.Equal(pos) {
		idx = -1
	}
	return idx
}

// getBlock returns a pending block by giving its index from findBlock method.
func (s *chainStatus) getBlock(idx int) (b *types.Block) {
	if idx < 0 || idx >= len(s.blocks) {
		return
	}
	b = s.blocks[idx]
	return
}

// addBlock adds a block to pending blocks on this chain.
func (s *chainStatus) addBlock(b *types.Block) error {
	s.blocks = append(s.blocks, b)
	s.tip = b
	return nil
}

// TODO(mission): change back to nextHeight.
// nextPosition returns a valid position for new block in this chain.
func (s *chainStatus) nextPosition() types.Position {
	if s.tip == nil {
		return types.Position{
			ChainID: s.ID,
			Height:  0,
		}
	}
	return types.Position{
		ChainID: s.ID,
		Height:  s.tip.Position.Height + 1,
	}
}

// purgeBlock purge a block from cache, make sure this block already
// persists to blockdb.
func (s *chainStatus) purgeBlock(b *types.Block) error {
	if b.Hash != s.blocks[0].Hash || s.nextOutputIndex <= 0 {
		return ErrPurgeNotDeliveredBlock
	}
	s.blocks = s.blocks[1:]
	s.nextOutputIndex--
	return nil
}