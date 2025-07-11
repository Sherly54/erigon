// Copyright 2024 The Erigon Authors
// This file is part of Erigon.
//
// Erigon is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// Erigon is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with Erigon. If not, see <http://www.gnu.org/licenses/>.

package transactions

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/holiman/uint256"

	"github.com/erigontech/erigon-lib/chain"
	"github.com/erigontech/erigon-lib/common"
	"github.com/erigontech/erigon-lib/kv"
	"github.com/erigontech/erigon-lib/log/v3"
	"github.com/erigontech/erigon-lib/types"
	"github.com/erigontech/erigon/core"
	"github.com/erigontech/erigon/core/state"
	"github.com/erigontech/erigon/core/vm"
	"github.com/erigontech/erigon/core/vm/evmtypes"
	"github.com/erigontech/erigon/execution/consensus"
	"github.com/erigontech/erigon/rpc"
	ethapi2 "github.com/erigontech/erigon/rpc/ethapi"
	"github.com/erigontech/erigon/turbo/services"
)

func DoCall(
	ctx context.Context,
	engine consensus.EngineReader,
	args ethapi2.CallArgs,
	tx kv.Tx,
	blockNrOrHash rpc.BlockNumberOrHash,
	header *types.Header,
	overrides *ethapi2.StateOverrides,
	gasCap uint64,
	chainConfig *chain.Config,
	stateReader state.StateReader,
	headerReader services.HeaderReader,
	callTimeout time.Duration,
) (*evmtypes.ExecutionResult, error) {
	// todo: Pending state is only known by the miner
	/*
		if blockNrOrHash.BlockNumber != nil && *blockNrOrHash.BlockNumber == rpc.PendingBlockNumber {
			block, state, _ := b.eth.miner.Pending()
			return state, block.Header(), nil
		}
	*/

	state := state.New(stateReader)

	// Override the fields of specified contracts before execution.
	if overrides != nil {
		if err := overrides.Override(state); err != nil {
			return nil, err
		}
	}

	// Setup context so it may be cancelled the call has completed
	// or, in case of unmetered gas, setup a context with a timeout.
	var cancel context.CancelFunc
	if callTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, callTimeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}

	// Make sure the context is cancelled when the call has completed
	// this makes sure resources are cleaned up.
	defer cancel()

	// Get a new instance of the EVM.
	var baseFee *uint256.Int
	if header != nil && header.BaseFee != nil {
		var overflow bool
		baseFee, overflow = uint256.FromBig(header.BaseFee)
		if overflow {
			return nil, errors.New("header.BaseFee uint256 overflow")
		}
	}
	msg, err := args.ToMessage(gasCap, baseFee)
	if err != nil {
		return nil, err
	}
	blockCtx := NewEVMBlockContext(engine, header, blockNrOrHash.RequireCanonical, tx, headerReader, chainConfig)
	txCtx := core.NewEVMTxContext(msg)

	evm := vm.NewEVM(blockCtx, txCtx, state, chainConfig, vm.Config{NoBaseFee: true})

	// Wait for the context to be done and cancel the evm. Even if the
	// EVM has finished, cancelling may be done (repeatedly)
	go func() {
		<-ctx.Done()
		evm.Cancel()
	}()

	gp := new(core.GasPool).AddGas(msg.Gas()).AddBlobGas(msg.BlobGas())
	result, err := core.ApplyMessage(evm, msg, gp, true /* refunds */, false /* gasBailout */, engine)
	if err != nil {
		return nil, err
	}

	// If the timer caused an abort, return an appropriate error message
	if evm.Cancelled() {
		return nil, fmt.Errorf("execution aborted (timeout = %v)", callTimeout)
	}
	return result, nil
}

func NewEVMBlockContext(engine consensus.EngineReader, header *types.Header, requireCanonical bool, tx kv.Getter,
	headerReader services.HeaderReader, config *chain.Config) evmtypes.BlockContext {
	blockHashFunc := MakeHeaderGetter(requireCanonical, tx, headerReader)
	return core.NewEVMBlockContext(header, blockHashFunc, engine, nil /* author */, config)
}

func MakeHeaderGetter(requireCanonical bool, tx kv.Getter, headerReader services.HeaderReader) func(uint64) (common.Hash, error) {
	return func(n uint64) (common.Hash, error) {
		h, err := headerReader.HeaderByNumber(context.Background(), tx, n)
		if err != nil {
			log.Error("Can't get block hash by number", "number", n, "only-canonical", requireCanonical)
			return common.Hash{}, err
		}
		if h == nil {
			log.Warn("[evm] header is nil", "blockNum", n)
			return common.Hash{}, nil
		}
		return h.Hash(), nil
	}
}

type ReusableCaller struct {
	evm             *vm.EVM
	intraBlockState *state.IntraBlockState
	gasCap          uint64
	baseFee         *uint256.Int
	stateReader     state.StateReader
	callTimeout     time.Duration
	message         *types.Message
}

func (r *ReusableCaller) DoCallWithNewGas(
	ctx context.Context,
	newGas uint64,
	engine consensus.EngineReader,
	overrides *ethapi2.StateOverrides,
) (*evmtypes.ExecutionResult, error) {
	var cancel context.CancelFunc
	if r.callTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, r.callTimeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}

	// Make sure the context is cancelled when the call has completed
	// this makes sure resources are cleaned up.
	defer cancel()

	r.message.ChangeGas(r.gasCap, newGas)

	// reset the EVM so that we can continue to use it with the new context
	txCtx := core.NewEVMTxContext(r.message)
	if overrides == nil {
		r.intraBlockState = state.New(r.stateReader)
	}

	r.evm.Reset(txCtx, r.intraBlockState)

	timedOut := false
	go func() {
		<-ctx.Done()
		timedOut = true
	}()

	gp := new(core.GasPool).AddGas(r.message.Gas()).AddBlobGas(r.message.BlobGas())

	result, err := core.ApplyMessage(r.evm, r.message, gp, true /* refunds */, false /* gasBailout */, engine)
	if err != nil {
		return nil, err
	}

	// If the timer caused an abort, return an appropriate error message
	if timedOut {
		return nil, fmt.Errorf("execution aborted (timeout = %v)", r.callTimeout)
	}

	return result, nil
}

func NewReusableCaller(
	engine consensus.EngineReader,
	stateReader state.StateReader,
	overrides *ethapi2.StateOverrides,
	header *types.Header,
	initialArgs ethapi2.CallArgs,
	gasCap uint64,
	blockNrOrHash rpc.BlockNumberOrHash,
	tx kv.Tx,
	headerReader services.HeaderReader,
	chainConfig *chain.Config,
	callTimeout time.Duration,
) (*ReusableCaller, error) {
	ibs := state.New(stateReader)

	if overrides != nil {
		if err := overrides.Override(ibs); err != nil {
			return nil, err
		}
	}

	var baseFee *uint256.Int
	if header != nil && header.BaseFee != nil {
		var overflow bool
		baseFee, overflow = uint256.FromBig(header.BaseFee)
		if overflow {
			return nil, errors.New("header.BaseFee uint256 overflow")
		}
	}

	msg, err := initialArgs.ToMessage(gasCap, baseFee)
	if err != nil {
		return nil, err
	}

	blockCtx := NewEVMBlockContext(engine, header, blockNrOrHash.RequireCanonical, tx, headerReader, chainConfig)
	txCtx := core.NewEVMTxContext(msg)

	evm := vm.NewEVM(blockCtx, txCtx, ibs, chainConfig, vm.Config{NoBaseFee: true})

	return &ReusableCaller{
		evm:             evm,
		intraBlockState: ibs,
		baseFee:         baseFee,
		gasCap:          gasCap,
		callTimeout:     callTimeout,
		stateReader:     stateReader,
		message:         msg,
	}, nil
}
