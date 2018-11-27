// Copyright 2018 The dexon-consensus Authors
// This file is part of the dexon-consensus library.
//
// The dexon-consensus library is free software: you can redistribute it
// and/or modify it under the terms of the GNU Lesser General Public License as
// published by the Free Software Foundation, either version 3 of the License,
// or (at your option) any later version.
//
// The dexon-consensus library is distributed in the hope that it will be
// useful, but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the GNU Lesser
// General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the dexon-consensus library. If not, see
// <http://www.gnu.org/licenses/>.

package core

import (
	"context"
	"encoding/hex"
	"fmt"
	"sync"
	"time"

	"github.com/dexon-foundation/dexon-consensus/common"
	"github.com/dexon-foundation/dexon-consensus/core/blockdb"
	"github.com/dexon-foundation/dexon-consensus/core/crypto"
	"github.com/dexon-foundation/dexon-consensus/core/types"
	typesDKG "github.com/dexon-foundation/dexon-consensus/core/types/dkg"
	"github.com/dexon-foundation/dexon-consensus/core/utils"
)

// Errors for consensus core.
var (
	ErrProposerNotInNodeSet = fmt.Errorf(
		"proposer is not in node set")
	ErrIncorrectHash = fmt.Errorf(
		"hash of block is incorrect")
	ErrIncorrectSignature = fmt.Errorf(
		"signature of block is incorrect")
	ErrGenesisBlockNotEmpty = fmt.Errorf(
		"genesis block should be empty")
	ErrUnknownBlockProposed = fmt.Errorf(
		"unknown block is proposed")
	ErrIncorrectAgreementResultPosition = fmt.Errorf(
		"incorrect agreement result position")
	ErrNotEnoughVotes = fmt.Errorf(
		"not enought votes")
	ErrIncorrectVoteBlockHash = fmt.Errorf(
		"incorrect vote block hash")
	ErrIncorrectVoteType = fmt.Errorf(
		"incorrect vote type")
	ErrIncorrectVotePosition = fmt.Errorf(
		"incorrect vote position")
	ErrIncorrectVoteProposer = fmt.Errorf(
		"incorrect vote proposer")
	ErrCRSNotReady = fmt.Errorf(
		"CRS not ready")
)

// consensusBAReceiver implements agreementReceiver.
type consensusBAReceiver struct {
	// TODO(mission): consensus would be replaced by lattice and network.
	consensus        *Consensus
	agreementModule  *agreement
	chainID          uint32
	changeNotaryTime time.Time
	round            uint64
	restartNotary    chan bool
}

func (recv *consensusBAReceiver) ProposeVote(vote *types.Vote) {
	if err := recv.agreementModule.prepareVote(vote); err != nil {
		recv.consensus.logger.Error("Failed to prepare vote", "error", err)
		return
	}
	go func() {
		if err := recv.agreementModule.processVote(vote); err != nil {
			recv.consensus.logger.Error("Failed to process vote", "error", err)
			return
		}
		recv.consensus.logger.Debug("Calling Network.BroadcastVote",
			"vote", vote)
		recv.consensus.network.BroadcastVote(vote)
	}()
}

func (recv *consensusBAReceiver) ProposeBlock() common.Hash {
	block := recv.consensus.proposeBlock(recv.chainID, recv.round)
	if block == nil {
		recv.consensus.logger.Error("unable to propose block")
		return nullBlockHash
	}
	if err := recv.consensus.preProcessBlock(block); err != nil {
		recv.consensus.logger.Error("Failed to pre-process block", "error", err)
		return common.Hash{}
	}
	recv.consensus.logger.Debug("Calling Network.BroadcastBlock", "block", block)
	recv.consensus.network.BroadcastBlock(block)
	return block.Hash
}

func (recv *consensusBAReceiver) ConfirmBlock(
	hash common.Hash, votes map[types.NodeID]*types.Vote) {
	var block *types.Block
	isEmptyBlockConfirmed := hash == common.Hash{}
	if isEmptyBlockConfirmed {
		aID := recv.agreementModule.agreementID()
		recv.consensus.logger.Info("Empty block is confirmed",
			"position", &aID)
		var err error
		block, err = recv.consensus.proposeEmptyBlock(recv.round, recv.chainID)
		if err != nil {
			recv.consensus.logger.Error("Propose empty block failed", "error", err)
			return
		}
	} else {
		var exist bool
		block, exist = recv.agreementModule.findCandidateBlockNoLock(hash)
		if !exist {
			recv.consensus.logger.Error("Unknown block confirmed",
				"hash", hash,
				"chainID", recv.chainID)
			ch := make(chan *types.Block)
			func() {
				recv.consensus.lock.Lock()
				defer recv.consensus.lock.Unlock()
				recv.consensus.baConfirmedBlock[hash] = ch
			}()
			recv.consensus.network.PullBlocks(common.Hashes{hash})
			go func() {
				block = <-ch
				recv.consensus.logger.Info("Receive unknown block",
					"hash", hash,
					"chainID", recv.chainID)
				recv.agreementModule.addCandidateBlock(block)
				recv.agreementModule.lock.Lock()
				defer recv.agreementModule.lock.Unlock()
				recv.ConfirmBlock(block.Hash, votes)
			}()
			return
		}
	}
	recv.consensus.ccModule.registerBlock(block)
	if block.Position.Height != 0 &&
		!recv.consensus.lattice.Exist(block.ParentHash) {
		go func(hash common.Hash) {
			parentHash := hash
			for {
				recv.consensus.logger.Warn("Parent block not confirmed",
					"hash", parentHash,
					"chainID", recv.chainID)
				ch := make(chan *types.Block)
				if !func() bool {
					recv.consensus.lock.Lock()
					defer recv.consensus.lock.Unlock()
					if _, exist := recv.consensus.baConfirmedBlock[parentHash]; exist {
						return false
					}
					recv.consensus.baConfirmedBlock[parentHash] = ch
					return true
				}() {
					return
				}
				var block *types.Block
			PullBlockLoop:
				for {
					recv.consensus.logger.Debug("Calling Network.PullBlock for parent",
						"hash", parentHash)
					recv.consensus.network.PullBlocks(common.Hashes{parentHash})
					select {
					case block = <-ch:
						break PullBlockLoop
					case <-time.After(1 * time.Second):
					}
				}
				recv.consensus.logger.Info("Receive parent block",
					"hash", block.ParentHash,
					"chainID", recv.chainID)
				recv.consensus.ccModule.registerBlock(block)
				if err := recv.consensus.processBlock(block); err != nil {
					recv.consensus.logger.Error("Failed to process block", "error", err)
					return
				}
				parentHash = block.ParentHash
				if block.Position.Height == 0 ||
					recv.consensus.lattice.Exist(parentHash) {
					return
				}
			}
		}(block.ParentHash)
	}
	voteList := make([]types.Vote, 0, len(votes))
	for _, vote := range votes {
		if vote.BlockHash != hash {
			continue
		}
		voteList = append(voteList, *vote)
	}
	result := &types.AgreementResult{
		BlockHash:    block.Hash,
		Position:     block.Position,
		Votes:        voteList,
		IsEmptyBlock: isEmptyBlockConfirmed,
	}
	recv.consensus.logger.Debug("Propose AgreementResult",
		"result", result)
	recv.consensus.network.BroadcastAgreementResult(result)
	if err := recv.consensus.processBlock(block); err != nil {
		recv.consensus.logger.Error("Failed to process block", "error", err)
		return
	}
	// Clean the restartNotary channel so BA will not stuck by deadlock.
CleanChannelLoop:
	for {
		select {
		case <-recv.restartNotary:
		default:
			break CleanChannelLoop
		}
	}
	if block.Timestamp.After(recv.changeNotaryTime) {
		recv.round++
		recv.restartNotary <- true
	} else {
		recv.restartNotary <- false
	}
}

func (recv *consensusBAReceiver) PullBlocks(hashes common.Hashes) {
	recv.consensus.logger.Debug("Calling Network.PullBlocks", "hashes", hashes)
	recv.consensus.network.PullBlocks(hashes)
}

// consensusDKGReceiver implements dkgReceiver.
type consensusDKGReceiver struct {
	ID           types.NodeID
	gov          Governance
	authModule   *Authenticator
	nodeSetCache *utils.NodeSetCache
	cfgModule    *configurationChain
	network      Network
	logger       common.Logger
}

// ProposeDKGComplaint proposes a DKGComplaint.
func (recv *consensusDKGReceiver) ProposeDKGComplaint(
	complaint *typesDKG.Complaint) {
	if err := recv.authModule.SignDKGComplaint(complaint); err != nil {
		recv.logger.Error("Failed to sign DKG complaint", "error", err)
		return
	}
	recv.logger.Debug("Calling Governace.AddDKGComplaint",
		"complaint", complaint)
	recv.gov.AddDKGComplaint(complaint.Round, complaint)
}

// ProposeDKGMasterPublicKey propose a DKGMasterPublicKey.
func (recv *consensusDKGReceiver) ProposeDKGMasterPublicKey(
	mpk *typesDKG.MasterPublicKey) {
	if err := recv.authModule.SignDKGMasterPublicKey(mpk); err != nil {
		recv.logger.Error("Failed to sign DKG master public key", "error", err)
		return
	}
	recv.logger.Debug("Calling Governance.AddDKGMasterPublicKey", "key", mpk)
	recv.gov.AddDKGMasterPublicKey(mpk.Round, mpk)
}

// ProposeDKGPrivateShare propose a DKGPrivateShare.
func (recv *consensusDKGReceiver) ProposeDKGPrivateShare(
	prv *typesDKG.PrivateShare) {
	if err := recv.authModule.SignDKGPrivateShare(prv); err != nil {
		recv.logger.Error("Failed to sign DKG private share", "error", err)
		return
	}
	receiverPubKey, exists := recv.nodeSetCache.GetPublicKey(prv.ReceiverID)
	if !exists {
		recv.logger.Error("Public key for receiver not found",
			"receiver", prv.ReceiverID.String()[:6])
		return
	}
	if prv.ReceiverID == recv.ID {
		go func() {
			if err := recv.cfgModule.processPrivateShare(prv); err != nil {
				recv.logger.Error("Failed to process self private share", "prvShare", prv)
			}
		}()
	} else {
		recv.logger.Debug("Calling Network.SendDKGPrivateShare",
			"receiver", hex.EncodeToString(receiverPubKey.Bytes()))
		recv.network.SendDKGPrivateShare(receiverPubKey, prv)
	}
}

// ProposeDKGAntiNackComplaint propose a DKGPrivateShare as an anti complaint.
func (recv *consensusDKGReceiver) ProposeDKGAntiNackComplaint(
	prv *typesDKG.PrivateShare) {
	if prv.ProposerID == recv.ID {
		if err := recv.authModule.SignDKGPrivateShare(prv); err != nil {
			recv.logger.Error("Failed sign DKG private share", "error", err)
			return
		}
	}
	recv.logger.Debug("Calling Network.BroadcastDKGPrivateShare", "share", prv)
	recv.network.BroadcastDKGPrivateShare(prv)
}

// ProposeDKGFinalize propose a DKGFinalize message.
func (recv *consensusDKGReceiver) ProposeDKGFinalize(final *typesDKG.Finalize) {
	if err := recv.authModule.SignDKGFinalize(final); err != nil {
		recv.logger.Error("Faield to sign DKG finalize", "error", err)
		return
	}
	recv.logger.Debug("Calling Governance.AddDKGFinalize", "final", final)
	recv.gov.AddDKGFinalize(final.Round, final)
}

// Consensus implements DEXON Consensus algorithm.
type Consensus struct {
	// Node Info.
	ID         types.NodeID
	authModule *Authenticator

	// BA.
	baMgr            *agreementMgr
	baConfirmedBlock map[common.Hash]chan<- *types.Block

	// DKG.
	dkgRunning int32
	dkgReady   *sync.Cond
	cfgModule  *configurationChain

	// Dexon consensus v1's modules.
	lattice  *Lattice
	ccModule *compactionChain
	toSyncer *totalOrderingSyncer

	// Interfaces.
	db        blockdb.BlockDatabase
	app       Application
	gov       Governance
	network   Network
	tickerObj Ticker

	// Misc.
	dMoment       time.Time
	nodeSetCache  *utils.NodeSetCache
	round         uint64
	roundToNotify uint64
	lock          sync.RWMutex
	ctx           context.Context
	ctxCancel     context.CancelFunc
	event         *common.Event
	logger        common.Logger
}

// NewConsensus construct an Consensus instance.
func NewConsensus(
	dMoment time.Time,
	app Application,
	gov Governance,
	db blockdb.BlockDatabase,
	network Network,
	prv crypto.PrivateKey,
	logger common.Logger) *Consensus {

	// TODO(w): load latest blockHeight from DB, and use config at that height.
	var round uint64
	logger.Debug("Calling Governance.Configuration", "round", round)
	config := gov.Configuration(round)
	nodeSetCache := utils.NewNodeSetCache(gov)
	logger.Debug("Calling Governance.CRS", "round", round)
	// Setup auth module.
	authModule := NewAuthenticator(prv)
	// Check if the application implement Debug interface.
	debugApp, _ := app.(Debug)
	// Init lattice.
	lattice := NewLattice(
		dMoment, round, config, authModule, app, debugApp, db, logger)
	// Init configuration chain.
	ID := types.NewNodeID(prv.PublicKey())
	recv := &consensusDKGReceiver{
		ID:           ID,
		gov:          gov,
		authModule:   authModule,
		nodeSetCache: nodeSetCache,
		network:      network,
		logger:       logger,
	}
	cfgModule := newConfigurationChain(
		ID,
		recv,
		gov,
		nodeSetCache,
		logger)
	recv.cfgModule = cfgModule
	// Construct Consensus instance.
	con := &Consensus{
		ID:               ID,
		ccModule:         newCompactionChain(gov),
		lattice:          lattice,
		app:              app,
		gov:              gov,
		db:               db,
		network:          network,
		tickerObj:        newTicker(gov, round, TickerBA),
		baConfirmedBlock: make(map[common.Hash]chan<- *types.Block),
		dkgReady:         sync.NewCond(&sync.Mutex{}),
		cfgModule:        cfgModule,
		dMoment:          dMoment,
		nodeSetCache:     nodeSetCache,
		authModule:       authModule,
		event:            common.NewEvent(),
		logger:           logger,
	}
	con.ctx, con.ctxCancel = context.WithCancel(context.Background())
	con.baMgr = newAgreementMgr(con, dMoment)
	return con
}

// Run starts running DEXON Consensus.
func (con *Consensus) Run(initBlock *types.Block) {
	// The block past from full node should be delivered already or known by
	// full node. We don't have to notify it.
	con.roundToNotify = initBlock.Position.Round + 1
	initRound := initBlock.Position.Round
	con.logger.Debug("Calling Governance.Configuration", "round", initRound)
	initConfig := con.gov.Configuration(initRound)
	// Setup context.
	con.ccModule.init(initBlock)
	// TODO(jimmy-dexon): change AppendConfig to add config for specific round.
	for i := uint64(0); i <= initRound+1; i++ {
		con.logger.Debug("Calling Governance.Configuration", "round", i)
		cfg := con.gov.Configuration(i)
		// 0 round is already given to core.Lattice module when constructing.
		if i > 0 {
			if err := con.lattice.AppendConfig(i, cfg); err != nil {
				panic(err)
			}
		}
		// Corresponding CRS might not be ready for next round to initRound.
		if i < initRound+1 {
			con.logger.Debug("Calling Governance.CRS", "round", i)
			crs := con.gov.CRS(i)
			if (crs == common.Hash{}) {
				panic(ErrCRSNotReady)
			}
			if err := con.baMgr.appendConfig(i, cfg, crs); err != nil {
				panic(err)
			}
		}
	}
	dkgSet, err := con.nodeSetCache.GetDKGSet(initRound)
	if err != nil {
		panic(err)
	}
	con.logger.Debug("Calling Network.ReceiveChan")
	go con.processMsg(con.network.ReceiveChan())
	// Sleep until dMoment come.
	time.Sleep(con.dMoment.Sub(time.Now().UTC()))
	if _, exist := dkgSet[con.ID]; exist {
		con.logger.Info("Selected as DKG set", "round", initRound)
		con.cfgModule.registerDKG(initRound, int(initConfig.DKGSetSize)/3+1)
		con.event.RegisterTime(con.dMoment.Add(initConfig.RoundInterval/4),
			func(time.Time) {
				con.runDKG(initRound, initConfig)
			})
	}
	con.initialRound(con.dMoment, initRound, initConfig)
	// Block until done.
	select {
	case <-con.ctx.Done():
	}
}

// runDKG starts running DKG protocol.
func (con *Consensus) runDKG(round uint64, config *types.Config) {
	con.dkgReady.L.Lock()
	defer con.dkgReady.L.Unlock()
	if con.dkgRunning != 0 {
		return
	}
	con.dkgRunning = 1
	go func() {
		startTime := time.Now().UTC()
		defer func() {
			con.dkgReady.L.Lock()
			defer con.dkgReady.L.Unlock()
			con.dkgReady.Broadcast()
			con.dkgRunning = 2
			DKGTime := time.Now().Sub(startTime)
			if DKGTime.Nanoseconds() >=
				config.RoundInterval.Nanoseconds()/2 {
				con.logger.Warn("Your computer cannot finish DKG on time!",
					"nodeID", con.ID.String())
			}
		}()
		if err := con.cfgModule.runDKG(round); err != nil {
			con.logger.Error("Failed to runDKG", "error", err)
		}
	}()
}

func (con *Consensus) runCRS(round uint64) {
	con.logger.Debug("Calling Governance.CRS to check if already proposed",
		"round", round+1)
	if (con.gov.CRS(round+1) != common.Hash{}) {
		con.logger.Info("CRS already proposed", "round", round+1)
		return
	}
	// Start running next round CRS.
	con.logger.Debug("Calling Governance.CRS", "round", round)
	psig, err := con.cfgModule.preparePartialSignature(round, con.gov.CRS(round))
	if err != nil {
		con.logger.Error("Failed to prepare partial signature", "error", err)
	} else if err = con.authModule.SignDKGPartialSignature(psig); err != nil {
		con.logger.Error("Failed to sign DKG partial signature", "error", err)
	} else if err = con.cfgModule.processPartialSignature(psig); err != nil {
		con.logger.Error("Failed to process partial signature", "error", err)
	} else {
		con.logger.Debug("Calling Network.BroadcastDKGPartialSignature",
			"proposer", psig.ProposerID,
			"round", psig.Round,
			"hash", psig.Hash)
		con.network.BroadcastDKGPartialSignature(psig)
		con.logger.Debug("Calling Governance.CRS", "round", round)
		crs, err := con.cfgModule.runCRSTSig(round, con.gov.CRS(round))
		if err != nil {
			con.logger.Error("Failed to run CRS Tsig", "error", err)
		} else {
			con.logger.Debug("Calling Governance.ProposeCRS",
				"round", round+1,
				"crs", hex.EncodeToString(crs))
			con.gov.ProposeCRS(round+1, crs)
		}
	}
}

func (con *Consensus) initialRound(
	startTime time.Time, round uint64, config *types.Config) {
	select {
	case <-con.ctx.Done():
		return
	default:
	}
	curDkgSet, err := con.nodeSetCache.GetDKGSet(round)
	if err != nil {
		con.logger.Error("Error getting DKG set", "round", round, "error", err)
		curDkgSet = make(map[types.NodeID]struct{})
	}
	// Initiate CRS routine.
	if _, exist := curDkgSet[con.ID]; exist {
		con.event.RegisterTime(startTime.Add(config.RoundInterval/2),
			func(time.Time) {
				go func() {
					con.runCRS(round)
				}()
			})
	}
	// Initiate BA modules.
	con.event.RegisterTime(
		startTime.Add(config.RoundInterval/2+config.LambdaDKG),
		func(time.Time) {
			go func(nextRound uint64) {
				for (con.gov.CRS(nextRound) == common.Hash{}) {
					con.logger.Info("CRS is not ready yet. Try again later...",
						"nodeID", con.ID,
						"round", nextRound)
					time.Sleep(500 * time.Millisecond)
				}
				// Notify BA for new round.
				con.logger.Debug("Calling Governance.Configuration",
					"round", nextRound)
				nextConfig := con.gov.Configuration(nextRound)
				con.logger.Debug("Calling Governance.CRS",
					"round", nextRound)
				nextCRS := con.gov.CRS(nextRound)
				if err := con.baMgr.appendConfig(
					nextRound, nextConfig, nextCRS); err != nil {
					panic(err)
				}
			}(round + 1)
		})
	// Initiate DKG for this round.
	con.event.RegisterTime(startTime.Add(config.RoundInterval/2+config.LambdaDKG),
		func(time.Time) {
			go func(nextRound uint64) {
				// Normally, gov.CRS would return non-nil. Use this for in case of
				// unexpected network fluctuation and ensure the robustness.
				for (con.gov.CRS(nextRound) == common.Hash{}) {
					con.logger.Info("CRS is not ready yet. Try again later...",
						"nodeID", con.ID,
						"round", nextRound)
					time.Sleep(500 * time.Millisecond)
				}
				nextDkgSet, err := con.nodeSetCache.GetDKGSet(nextRound)
				if err != nil {
					con.logger.Error("Error getting DKG set",
						"round", nextRound,
						"error", err)
					return
				}
				if _, exist := nextDkgSet[con.ID]; !exist {
					return
				}
				con.logger.Info("Selected as DKG set", "round", nextRound)
				con.cfgModule.registerDKG(
					nextRound, int(config.DKGSetSize/3)+1)
				con.event.RegisterTime(
					startTime.Add(config.RoundInterval*2/3),
					func(time.Time) {
						func() {
							con.dkgReady.L.Lock()
							defer con.dkgReady.L.Unlock()
							con.dkgRunning = 0
						}()
						con.logger.Debug("Calling Governance.Configuration",
							"round", nextRound)
						nextConfig := con.gov.Configuration(nextRound)
						con.runDKG(nextRound, nextConfig)
					})
			}(round + 1)
		})
	// Prepare lattice module for next round and next "initialRound" routine.
	con.event.RegisterTime(startTime.Add(config.RoundInterval),
		func(time.Time) {
			// Change round.
			// Get configuration for next round.
			nextRound := round + 1
			con.logger.Debug("Calling Governance.Configuration",
				"round", nextRound)
			nextConfig := con.gov.Configuration(nextRound)
			con.initialRound(
				startTime.Add(config.RoundInterval), nextRound, nextConfig)
		})
}

// Stop the Consensus core.
func (con *Consensus) Stop() {
	con.baMgr.stop()
	con.event.Reset()
	con.ctxCancel()
}

func (con *Consensus) processMsg(msgChan <-chan interface{}) {
MessageLoop:
	for {
		var msg interface{}
		select {
		case msg = <-msgChan:
		case <-con.ctx.Done():
			return
		}

		switch val := msg.(type) {
		case *types.Block:
			if ch, exist := func() (chan<- *types.Block, bool) {
				con.lock.RLock()
				defer con.lock.RUnlock()
				ch, e := con.baConfirmedBlock[val.Hash]
				return ch, e
			}(); exist {
				if err := con.lattice.SanityCheck(val); err != nil {
					if err == ErrRetrySanityCheckLater {
						err = nil
					} else {
						con.logger.Error("SanityCheck failed", "error", err)
						continue MessageLoop
					}
				}
				func() {
					con.lock.Lock()
					defer con.lock.Unlock()
					// In case of multiple delivered block.
					if _, exist := con.baConfirmedBlock[val.Hash]; !exist {
						return
					}
					delete(con.baConfirmedBlock, val.Hash)
					ch <- val
				}()
			} else if val.IsFinalized() {
				// For sync mode.
				if err := con.processFinalizedBlock(val); err != nil {
					con.logger.Error("Failed to process finalized block",
						"error", err)
				}
			} else {
				if err := con.preProcessBlock(val); err != nil {
					con.logger.Error("Failed to pre process block",
						"error", err)
				}
			}
		case *types.Vote:
			if err := con.ProcessVote(val); err != nil {
				con.logger.Error("Failed to process vote",
					"error", err)
			}
		case *types.AgreementResult:
			if err := con.ProcessAgreementResult(val); err != nil {
				con.logger.Error("Failed to process agreement result",
					"error", err)
			}
		case *types.BlockRandomnessResult:
			if err := con.ProcessBlockRandomnessResult(val); err != nil {
				con.logger.Error("Failed to process block randomness result",
					"error", err)
			}
		case *typesDKG.PrivateShare:
			if err := con.cfgModule.processPrivateShare(val); err != nil {
				con.logger.Error("Failed to process private share",
					"error", err)
			}

		case *typesDKG.PartialSignature:
			if err := con.cfgModule.processPartialSignature(val); err != nil {
				con.logger.Error("Failed to process partial signature",
					"error", err)
			}
		}
	}
}

func (con *Consensus) proposeBlock(chainID uint32, round uint64) *types.Block {
	block := &types.Block{
		Position: types.Position{
			ChainID: chainID,
			Round:   round,
		},
	}
	if err := con.prepareBlock(block, time.Now().UTC()); err != nil {
		con.logger.Error("Failed to prepare block", "error", err)
		return nil
	}
	return block
}

func (con *Consensus) proposeEmptyBlock(
	round uint64, chainID uint32) (*types.Block, error) {
	block := &types.Block{
		Position: types.Position{
			Round:   round,
			ChainID: chainID,
		},
	}
	if err := con.lattice.PrepareEmptyBlock(block); err != nil {
		return nil, err
	}
	return block, nil
}

// ProcessVote is the entry point to submit ont vote to a Consensus instance.
func (con *Consensus) ProcessVote(vote *types.Vote) (err error) {
	v := vote.Clone()
	err = con.baMgr.processVote(v)
	return
}

// ProcessAgreementResult processes the randomness request.
func (con *Consensus) ProcessAgreementResult(
	rand *types.AgreementResult) error {
	// Sanity Check.
	notarySet, err := con.nodeSetCache.GetNotarySet(
		rand.Position.Round, rand.Position.ChainID)
	if err != nil {
		return err
	}
	if len(rand.Votes) < len(notarySet)/3*2+1 {
		return ErrNotEnoughVotes
	}
	if len(rand.Votes) > len(notarySet) {
		return ErrIncorrectVoteProposer
	}
	for _, vote := range rand.Votes {
		if rand.IsEmptyBlock {
			if (vote.BlockHash != common.Hash{}) {
				return ErrIncorrectVoteBlockHash
			}
		} else {
			if vote.BlockHash != rand.BlockHash {
				return ErrIncorrectVoteBlockHash
			}
		}
		if vote.Type != types.VoteCom {
			return ErrIncorrectVoteType
		}
		if vote.Position != rand.Position {
			return ErrIncorrectVotePosition
		}
		if _, exist := notarySet[vote.ProposerID]; !exist {
			return ErrIncorrectVoteProposer
		}
		ok, err := verifyVoteSignature(&vote)
		if err != nil {
			return err
		}
		if !ok {
			return ErrIncorrectVoteSignature
		}
	}
	// Syncing BA Module.
	if err := con.baMgr.processAgreementResult(rand); err != nil {
		return err
	}
	// Calculating randomness.
	if rand.Position.Round == 0 {
		return nil
	}
	if !con.ccModule.blockRegistered(rand.BlockHash) {
		return nil
	}
	// Sanity check done.
	if !con.cfgModule.touchTSigHash(rand.BlockHash) {
		return nil
	}
	con.logger.Debug("Rebroadcast AgreementResult",
		"result", rand)
	con.network.BroadcastAgreementResult(rand)
	dkgSet, err := con.nodeSetCache.GetDKGSet(rand.Position.Round)
	if err != nil {
		return err
	}
	if _, exist := dkgSet[con.ID]; !exist {
		return nil
	}
	psig, err := con.cfgModule.preparePartialSignature(rand.Position.Round, rand.BlockHash)
	if err != nil {
		return err
	}
	if err = con.authModule.SignDKGPartialSignature(psig); err != nil {
		return err
	}
	if err = con.cfgModule.processPartialSignature(psig); err != nil {
		return err
	}
	con.logger.Debug("Calling Network.BroadcastDKGPartialSignature",
		"proposer", psig.ProposerID,
		"round", psig.Round,
		"hash", psig.Hash)
	con.network.BroadcastDKGPartialSignature(psig)
	go func() {
		tsig, err := con.cfgModule.runTSig(rand.Position.Round, rand.BlockHash)
		if err != nil {
			if err != ErrTSigAlreadyRunning {
				con.logger.Error("Faield to run TSIG", "error", err)
			}
			return
		}
		result := &types.BlockRandomnessResult{
			BlockHash:  rand.BlockHash,
			Position:   rand.Position,
			Randomness: tsig.Signature,
		}
		if err := con.ProcessBlockRandomnessResult(result); err != nil {
			con.logger.Error("Failed to process randomness result",
				"error", err)
			return
		}
	}()
	return nil
}

// ProcessBlockRandomnessResult processes the randomness result.
func (con *Consensus) ProcessBlockRandomnessResult(
	rand *types.BlockRandomnessResult) error {
	if rand.Position.Round == 0 {
		return nil
	}
	if err := con.ccModule.processBlockRandomnessResult(rand); err != nil {
		if err == ErrBlockNotRegistered {
			err = nil
		}
		return err
	}
	con.logger.Debug("Calling Network.BroadcastRandomnessResult",
		"hash", rand.BlockHash,
		"position", &rand.Position,
		"randomness", hex.EncodeToString(rand.Randomness))
	con.network.BroadcastRandomnessResult(rand)
	return nil
}

// preProcessBlock performs Byzantine Agreement on the block.
func (con *Consensus) preProcessBlock(b *types.Block) (err error) {
	err = con.baMgr.processBlock(b)
	return
}

// deliverBlock deliver a block to application layer.
func (con *Consensus) deliverBlock(b *types.Block) {
	con.logger.Debug("Calling Application.BlockDelivered", "block", b)
	con.app.BlockDelivered(b.Hash, b.Position, b.Finalization.Clone())
	if b.Position.Round == con.roundToNotify {
		// Get configuration for the round next to next round. Configuration
		// for that round should be ready at this moment and is required for
		// lattice module. This logic is related to:
		//  - roundShift
		//  - notifyGenesisRound
		futureRound := con.roundToNotify + 1
		con.logger.Debug("Calling Governance.Configuration",
			"round", con.roundToNotify)
		futureConfig := con.gov.Configuration(futureRound)
		con.logger.Debug("Append Config", "round", futureRound)
		if err := con.lattice.AppendConfig(
			futureRound, futureConfig); err != nil {
			con.logger.Debug("Unable to append config",
				"round", futureRound,
				"error", err)
			panic(err)
		}
		// Only the first block delivered of that round would
		// trigger this noitification.
		con.logger.Debug("Calling Governance.NotifyRoundHeight",
			"round", con.roundToNotify,
			"height", b.Finalization.Height)
		con.gov.NotifyRoundHeight(
			con.roundToNotify, b.Finalization.Height)
		con.roundToNotify++
	}
}

// processBlock is the entry point to submit one block to a Consensus instance.
func (con *Consensus) processBlock(block *types.Block) (err error) {
	if err = con.db.Put(*block); err != nil && err != blockdb.ErrBlockExists {
		return
	}
	con.lock.Lock()
	defer con.lock.Unlock()
	// Block processed by lattice can be out-of-order. But the output of lattice
	// (deliveredBlocks) cannot.
	deliveredBlocks, err := con.lattice.ProcessBlock(block)
	if err != nil {
		return
	}
	// Pass delivered blocks to compaction chain.
	for _, b := range deliveredBlocks {
		if err = con.ccModule.processBlock(b); err != nil {
			return
		}
		go con.event.NotifyTime(b.Finalization.Timestamp)
	}
	deliveredBlocks = con.ccModule.extractBlocks()
	con.logger.Debug("Last block in compaction chain",
		"block", con.ccModule.lastBlock())
	for _, b := range deliveredBlocks {
		if err = con.db.Update(*b); err != nil {
			panic(err)
		}
		con.cfgModule.untouchTSigHash(b.Hash)
		con.deliverBlock(b)
	}
	if err = con.lattice.PurgeBlocks(deliveredBlocks); err != nil {
		return
	}
	return
}

// processFinalizedBlock is the entry point for syncing blocks.
func (con *Consensus) processFinalizedBlock(block *types.Block) (err error) {
	if err = con.lattice.SanityCheck(block); err != nil {
		if err != ErrRetrySanityCheckLater {
			return
		}
		err = nil
	}
	con.ccModule.processFinalizedBlock(block)
	for {
		confirmed := con.ccModule.extractFinalizedBlocks()
		if len(confirmed) == 0 {
			break
		}
		if err = con.lattice.ctModule.processBlocks(confirmed); err != nil {
			return
		}
		for _, b := range confirmed {
			if err = con.db.Put(*b); err != nil {
				if err != blockdb.ErrBlockExists {
					return
				}
				err = nil
			}
			con.lattice.ProcessFinalizedBlock(b)
			// TODO(jimmy): BlockConfirmed and DeliverBlock may not be removed if
			// application implements state snapshot.
			con.logger.Debug("Calling Application.BlockConfirmed", "block", b)
			con.app.BlockConfirmed(*b.Clone())
			con.deliverBlock(b)
		}
	}
	return
}

// PrepareBlock would setup header fields of block based on its ProposerID.
func (con *Consensus) prepareBlock(b *types.Block,
	proposeTime time.Time) (err error) {
	if err = con.lattice.PrepareBlock(b, proposeTime); err != nil {
		return
	}
	con.logger.Debug("Calling Governance.CRS", "round", b.Position.Round)
	crs := con.gov.CRS(b.Position.Round)
	if crs.Equal(common.Hash{}) {
		con.logger.Error("CRS for round is not ready, unable to prepare block",
			"position", &b.Position)
		err = ErrCRSNotReady
		return
	}
	if err = con.authModule.SignCRS(b, crs); err != nil {
		return
	}
	return
}

// PrepareGenesisBlock would setup header fields for genesis block.
func (con *Consensus) PrepareGenesisBlock(b *types.Block,
	proposeTime time.Time) (err error) {
	if err = con.prepareBlock(b, proposeTime); err != nil {
		return
	}
	if len(b.Payload) != 0 {
		err = ErrGenesisBlockNotEmpty
		return
	}
	return
}
