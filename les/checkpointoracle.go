// Copyright 2019 The go-ethereum Authors
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

package les

import (
	"encoding/binary"
	"sync/atomic"

	"github.com/blessingreacoin/go-breadiuma/accounts/abi/bind"
	"github.com/blessingreacoin/go-breadiuma/common"
	"github.com/blessingreacoin/go-breadiuma/contracts/checkpointoracle"
	"github.com/blessingreacoin/go-breadiuma/crypto"
	"github.com/blessingreacoin/go-breadiuma/log"
	"github.com/blessingreacoin/go-breadiuma/params"
)

// checkpointOracle is responsible for offering the latest stable checkpoint
// generated and announced by the contract admins on-chain. The checkpoint is
// verified by clients locally during the checkpoint syncing.
type checkpointOracle struct {
	config   *params.CheckpointOracleConfig
	contract *checkpointoracle.CheckpointOracle

	// Whether the contract backend is set.
	running int32

	getLocal     func(uint64) params.TrustedCheckpoint // Function used to retrieve local checkpoint
	syncDoneHook func()                                // Function used to notify that light syncing has completed.
}

// newCheckpointOracle returns a checkpoint registrar handler.
func newCheckpointOracle(config *params.CheckpointOracleConfig, getLocal func(uint64) params.TrustedCheckpoint) *checkpointOracle {
	if config == nil {
		log.Info("Checkpoint registrar is not enabled")
		return nil
	}
	if config.Address == (common.Address{}) || uint64(len(config.Signers)) < config.Threshold {
		log.Warn("Invalid checkpoint registrar config")
		return nil
	}
	log.Info("Configured checkpoint registrar", "address", config.Address, "signers", len(config.Signers), "threshold", config.Threshold)

	return &checkpointOracle{
		config:   config,
		getLocal: getLocal,
	}
}

// start binds the registrar contract and start listening to the
// newCheckpointEvent for the server side.
func (reg *checkpointOracle) start(backend bind.ContractBackend) {
	contract, err := checkpointoracle.NewCheckpointOracle(reg.config.Address, backend)
	if err != nil {
		log.Error("Oracle contract binding failed", "err", err)
		return
	}
	if !atomic.CompareAndSwapInt32(&reg.running, 0, 1) {
		log.Error("Already bound and listening to registrar")
		return
	}
	reg.contract = contract
}

// isRunning returns an indicator whether the registrar is running.
func (reg *checkpointOracle) isRunning() bool {
	return atomic.LoadInt32(&reg.running) == 1
}

// stableCheckpoint returns the stable checkpoint which was generated by local
// indexers and announced by trusted signers.
func (reg *checkpointOracle) stableCheckpoint() (*params.TrustedCheckpoint, uint64) {
	// Retrieve the latest checkpoint from the contract, abort if empty
	latest, hash, height, err := reg.contract.Contract().GetLatestCheckpoint(nil)
	if err != nil || (latest == 0 && hash == [32]byte{}) {
		return nil, 0
	}
	local := reg.getLocal(latest)

	// The following scenarios may occur:
	//
	// * local node is out of sync so that it doesn't have the
	//   checkpoint which registered in the contract.
	// * local checkpoint doesn't match with the registered one.
	//
	// In both cases, server won't send the **stable** checkpoint
	// to the client(no worry, client can use hardcoded one instead).
	if local.HashEqual(common.Hash(hash)) {
		return &local, height.Uint64()
	}
	return nil, 0
}

// verifySigners recovers the signer addresses according to the signature and
// checks whether there are enough approvals to finalize the checkpoint.
func (reg *checkpointOracle) verifySigners(index uint64, hash [32]byte, signatures [][]byte) (bool, []common.Address) {
	// Short circuit if the given signatures doesn't reach the threshold.
	if len(signatures) < int(reg.config.Threshold) {
		return false, nil
	}
	var (
		signers []common.Address
		checked = make(map[common.Address]struct{})
	)
	for i := 0; i < len(signatures); i++ {
		if len(signatures[i]) != 65 {
			continue
		}
		// EIP 191 style signatures
		//
		// Arguments when calculating hash to validate
		// 1: byte(0x19) - the initial 0x19 byte
		// 2: byte(0) - the version byte (data with intended validator)
		// 3: this - the validator address
		// --  Application specific data
		// 4 : checkpoint section_index (uint64)
		// 5 : checkpoint hash (bytes32)
		//     hash = keccak256(checkpoint_index, section_head, cht_root, bloom_root)
		buf := make([]byte, 8)
		binary.BigEndian.PutUint64(buf, index)
		data := append([]byte{0x19, 0x00}, append(reg.config.Address.Bytes(), append(buf, hash[:]...)...)...)
		signatures[i][64] -= 27 // Transform V from 27/28 to 0/1 according to the yellow paper for verification.
		pubkey, err := crypto.Ecrecover(crypto.Keccak256(data), signatures[i])
		if err != nil {
			return false, nil
		}
		var signer common.Address
		copy(signer[:], crypto.Keccak256(pubkey[1:])[12:])
		if _, exist := checked[signer]; exist {
			continue
		}
		for _, s := range reg.config.Signers {
			if s == signer {
				signers = append(signers, signer)
				checked[signer] = struct{}{}
			}
		}
	}
	threshold := reg.config.Threshold
	if uint64(len(signers)) < threshold {
		log.Warn("Not enough signers to approve checkpoint", "signers", len(signers), "threshold", threshold)
		return false, nil
	}
	return true, signers
}
