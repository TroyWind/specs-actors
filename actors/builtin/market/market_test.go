package market_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"math/rand"
	"testing"

	"github.com/filecoin-project/go-address"
	"github.com/filecoin-project/specs-actors/actors/abi"
	"github.com/filecoin-project/specs-actors/actors/abi/big"
	"github.com/filecoin-project/specs-actors/actors/builtin"
	"github.com/filecoin-project/specs-actors/actors/builtin/market"
	"github.com/filecoin-project/specs-actors/actors/builtin/miner"
	"github.com/filecoin-project/specs-actors/actors/crypto"
	"github.com/filecoin-project/specs-actors/actors/runtime/exitcode"
	"github.com/filecoin-project/specs-actors/actors/util/adt"
	"github.com/filecoin-project/specs-actors/support/mock"
	tutil "github.com/filecoin-project/specs-actors/support/testing"

	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExports(t *testing.T) {
	mock.CheckActorExports(t, market.Actor{})
}

func TestRemoveAllError(t *testing.T) {
	marketActor := tutil.NewIDAddr(t, 100)
	builder := mock.NewBuilder(context.Background(), marketActor)
	rt := builder.Build(t)
	store := adt.AsStore(rt)

	smm := market.MakeEmptySetMultimap(store)

	if err := smm.RemoveAll(42); err != nil {
		t.Fatalf("expected no error, got: %s", err)
	}
}

func TestMarketActor(t *testing.T) {
	marketActor := tutil.NewIDAddr(t, 100)
	owner := tutil.NewIDAddr(t, 101)
	provider := tutil.NewIDAddr(t, 102)
	worker := tutil.NewIDAddr(t, 103)
	client := tutil.NewIDAddr(t, 104)
	var st market.State

	setup := func() (*mock.Runtime, *marketActorTestHarness) {
		builder := mock.NewBuilder(context.Background(), marketActor).
			WithCaller(builtin.SystemActorAddr, builtin.InitActorCodeID).
			WithActorType(owner, builtin.AccountActorCodeID).
			WithActorType(worker, builtin.AccountActorCodeID).
			WithActorType(provider, builtin.StorageMinerActorCodeID).
			WithActorType(client, builtin.AccountActorCodeID)

		rt := builder.Build(t)

		actor := marketActorTestHarness{t: t}
		actor.constructAndVerify(rt)

		return rt, &actor
	}

	t.Run("simple construction", func(t *testing.T) {
		actor := market.Actor{}
		receiver := tutil.NewIDAddr(t, 100)
		builder := mock.NewBuilder(context.Background(), receiver).
			WithCaller(builtin.SystemActorAddr, builtin.InitActorCodeID)

		rt := builder.Build(t)

		rt.ExpectValidateCallerAddr(builtin.SystemActorAddr)

		ret := rt.Call(actor.Constructor, nil).(*adt.EmptyValue)
		assert.Nil(t, ret)
		rt.Verify()

		store := adt.AsStore(rt)

		emptyMap, err := adt.MakeEmptyMap(store).Root()
		assert.NoError(t, err)

		emptyArray, err := adt.MakeEmptyArray(store).Root()
		assert.NoError(t, err)

		emptyMultiMap, err := market.MakeEmptySetMultimap(store).Root()
		assert.NoError(t, err)

		var state market.State
		rt.GetState(&state)

		assert.Equal(t, emptyArray, state.Proposals)
		assert.Equal(t, emptyArray, state.States)
		assert.Equal(t, emptyMap, state.EscrowTable)
		assert.Equal(t, emptyMap, state.LockedTable)
		assert.Equal(t, abi.DealID(0), state.NextID)
		assert.Equal(t, emptyMultiMap, state.DealOpsByEpoch)
		assert.Equal(t, abi.ChainEpoch(-1), state.LastCron)
	})

	t.Run("AddBalance", func(t *testing.T) {
		t.Run("adds to provider escrow funds", func(t *testing.T) {
			testCases := []struct {
				delta int64
				total int64
			}{
				{10, 10},
				{20, 30},
				{40, 70},
			}

			// Test adding provider funds from both worker and owner address
			for _, callerAddr := range []address.Address{owner, worker} {
				rt, actor := setup()

				for _, tc := range testCases {
					rt.SetCaller(callerAddr, builtin.AccountActorCodeID)
					rt.SetReceived(abi.NewTokenAmount(tc.delta))
					actor.expectProviderControlAddressesAndValidateCaller(rt, provider, owner, worker)

					rt.Call(actor.AddBalance, &provider)

					rt.Verify()

					rt.GetState(&st)
					assert.Equal(t, abi.NewTokenAmount(tc.total), st.GetEscrowBalance(rt, provider))
				}
			}
		})

		t.Run("fails unless called by an account actor", func(t *testing.T) {
			rt, actor := setup()

			rt.SetReceived(abi.NewTokenAmount(10))
			actor.expectProviderControlAddressesAndValidateCaller(rt, provider, owner, worker)

			rt.SetCaller(provider, builtin.StorageMinerActorCodeID)
			rt.ExpectAbort(exitcode.ErrForbidden, func() {
				rt.Call(actor.AddBalance, &provider)
			})

			rt.Verify()
		})

		t.Run("adds to non-provider escrow funds", func(t *testing.T) {
			testCases := []struct {
				delta int64
				total int64
			}{
				{10, 10},
				{20, 30},
				{40, 70},
			}

			// Test adding non-provider funds from both worker and client addresses
			for _, callerAddr := range []address.Address{client, worker} {
				rt, actor := setup()

				for _, tc := range testCases {
					rt.SetCaller(callerAddr, builtin.AccountActorCodeID)
					rt.SetReceived(abi.NewTokenAmount(tc.delta))
					rt.ExpectValidateCallerType(builtin.CallerTypesSignable...)

					rt.Call(actor.AddBalance, &callerAddr)

					rt.Verify()

					rt.GetState(&st)
					assert.Equal(t, abi.NewTokenAmount(tc.total), st.GetEscrowBalance(rt, callerAddr))
				}
			}
		})
	})

	t.Run("WithdrawBalance", func(t *testing.T) {
		startEpoch := abi.ChainEpoch(10)
		endEpoch := abi.ChainEpoch(20)
		publishEpoch := abi.ChainEpoch(5)

		t.Run("fails with a negative withdraw amount", func(t *testing.T) {
			rt, actor := setup()

			params := market.WithdrawBalanceParams{
				ProviderOrClientAddress: provider,
				Amount:                  abi.NewTokenAmount(-1),
			}

			rt.ExpectAbort(exitcode.ErrIllegalArgument, func() {
				rt.Call(actor.WithdrawBalance, &params)
			})

			rt.Verify()
		})

		t.Run("withdraws from provider escrow funds and sends to owner", func(t *testing.T) {
			rt, actor := setup()

			actor.addProviderFunds(rt, provider, owner, worker, abi.NewTokenAmount(20))

			rt.GetState(&st)
			assert.Equal(t, abi.NewTokenAmount(20), st.GetEscrowBalance(rt, provider))

			// worker calls WithdrawBalance, balance is transferred to owner
			withdrawAmount := abi.NewTokenAmount(1)
			actor.withdrawStorageMinerBalanceOK(rt, owner, worker, provider, withdrawAmount, withdrawAmount)

			rt.GetState(&st)
			assert.Equal(t, abi.NewTokenAmount(19), st.GetEscrowBalance(rt, provider))
		})

		t.Run("withdraws from non-provider escrow funds", func(t *testing.T) {
			rt, actor := setup()
			actor.addParticipantFunds(rt, client, abi.NewTokenAmount(20))

			rt.GetState(&st)
			assert.Equal(t, abi.NewTokenAmount(20), st.GetEscrowBalance(rt, client))

			withdrawAmount := abi.NewTokenAmount(1)
			actor.withdrawClientBalanceOK(rt, client, withdrawAmount, withdrawAmount)

			rt.GetState(&st)
			assert.Equal(t, abi.NewTokenAmount(19), st.GetEscrowBalance(rt, client))
		})

		t.Run("client withdrawing more than escrow balance limits to available funds", func(t *testing.T) {
			rt, actor := setup()
			actor.addParticipantFunds(rt, client, abi.NewTokenAmount(20))

			// withdraw amount greater than escrow balance
			withdrawAmount := abi.NewTokenAmount(25)
			expectedAmount := abi.NewTokenAmount(20)
			actor.withdrawClientBalanceOK(rt, client, withdrawAmount, expectedAmount)

			rt.GetState(&st)
			assert.Equal(t, abi.NewTokenAmount(0), st.GetEscrowBalance(rt, client))
		})

		t.Run("worker withdrawing more than escrow balance limits to available funds", func(t *testing.T) {
			rt, actor := setup()
			actor.addProviderFunds(rt, provider, owner, worker, abi.NewTokenAmount(20))

			rt.GetState(&st)
			assert.Equal(t, abi.NewTokenAmount(20), st.GetEscrowBalance(rt, provider))

			// withdraw amount greater than escrow balance
			withdrawAmount := abi.NewTokenAmount(25)
			actualWithdrawn := abi.NewTokenAmount(20)
			actor.withdrawStorageMinerBalanceOK(rt, owner, worker, provider, withdrawAmount, actualWithdrawn)

			rt.GetState(&st)
			assert.Equal(t, abi.NewTokenAmount(0), st.GetEscrowBalance(rt, provider))
		})

		t.Run("balance after withdrawal must ALWAYS be greater than or equal to locked amount", func(t *testing.T) {
			rt, actor := setup()

			// create the deal to publish
			deal := actor.generateUnVerifiedDealProposal(client, provider, startEpoch, endEpoch)

			// ensure client and provider have enough funds to lock for the deal
			actor.addParticipantFunds(rt, client, deal.ClientBalanceRequirement())
			actor.addProviderFunds(rt, provider, owner, worker, deal.ProviderBalanceRequirement())

			// publish the deal so that client AND provider collateral is locked
			rt.SetEpoch(publishEpoch)
			actor.publishDeal(rt, deal, owner, worker, provider)
			rt.GetState(&st)
			require.Equal(t, deal.ProviderCollateral, st.GetLockedBalance(rt, provider))
			require.Equal(t, deal.ClientBalanceRequirement(), st.GetLockedBalance(rt, client))

			withDrawAmt := abi.NewTokenAmount(1)
			withDrawableAmt := abi.NewTokenAmount(0)
			// client cannot withdraw any funds since all it's balance is locked
			actor.withdrawClientBalanceOK(rt, client, withDrawAmt, withDrawableAmt)
			//  provider cannot withdraw any funds since all it's balance is locked
			actor.withdrawStorageMinerBalanceOK(rt, owner, worker, provider, withDrawAmt, withDrawableAmt)

			// add some more funds to the provider & ensure withdrawal is limited by the locked funds
			withDrawAmt = abi.NewTokenAmount(30)
			withDrawableAmt = abi.NewTokenAmount(25)
			actor.addProviderFunds(rt, provider, owner, worker, withDrawableAmt)
			actor.withdrawStorageMinerBalanceOK(rt, owner, worker, provider, withDrawAmt, withDrawableAmt)

			// add some more funds to the client & ensure withdrawal is limited by the locked funds
			actor.addParticipantFunds(rt, client, withDrawableAmt)
			actor.withdrawClientBalanceOK(rt, client, withDrawAmt, withDrawableAmt)
		})

		t.Run("worker balance after withdrawal must account for slashed funds", func(t *testing.T) {
			rt, actor := setup()

			// create the deal to publish
			deal := actor.generateUnVerifiedDealProposal(client, provider, startEpoch, endEpoch)

			// ensure client and provider have enough funds to lock for the deal
			actor.addParticipantFunds(rt, client, deal.ClientBalanceRequirement())
			actor.addProviderFunds(rt, provider, owner, worker, deal.ProviderBalanceRequirement())

			// publish the deal
			rt.SetEpoch(publishEpoch)
			dealID := actor.publishDeal(rt, deal, owner, worker, provider)

			// activate the deal
			actor.activeDealOK(rt, dealID, endEpoch+1, provider)
			st := actor.mustGetDealState(rt, dealID)
			require.EqualValues(t, publishEpoch, st.SectorStartEpoch)

			// slash the deal
			rt.SetEpoch(publishEpoch + 1)
			actor.terminateDealOK(rt, dealID, provider)
			st = actor.mustGetDealState(rt, dealID)
			require.EqualValues(t, publishEpoch+1, st.SlashEpoch)

			// provider cannot withdraw any funds since all it's balance is locked
			withDrawAmt := abi.NewTokenAmount(1)
			actualWithdrawn := abi.NewTokenAmount(0)
			actor.withdrawStorageMinerBalanceOK(rt, owner, worker, provider, withDrawAmt, actualWithdrawn)

			// add some more funds to the provider & ensure withdrawal is limited by the locked funds
			actor.addProviderFunds(rt, provider, owner, worker, abi.NewTokenAmount(25))
			withDrawAmt = abi.NewTokenAmount(30)
			actualWithdrawn = abi.NewTokenAmount(25)

			actor.withdrawStorageMinerBalanceOK(rt, owner, worker, provider, withDrawAmt, actualWithdrawn)
		})
	})
}

type marketActorTestHarness struct {
	market.Actor
	t testing.TB
}

func (h *marketActorTestHarness) constructAndVerify(rt *mock.Runtime) {
	rt.ExpectValidateCallerAddr(builtin.SystemActorAddr)
	ret := rt.Call(h.Constructor, nil)
	assert.Nil(h.t, ret)
	rt.Verify()
}

// addProviderFunds is a helper method to setup provider market funds
func (h *marketActorTestHarness) addProviderFunds(rt *mock.Runtime, provider address.Address, owner address.Address, worker address.Address, amount abi.TokenAmount) {
	rt.SetReceived(amount)
	rt.SetCaller(owner, builtin.AccountActorCodeID)
	h.expectProviderControlAddressesAndValidateCaller(rt, provider, owner, worker)

	rt.Call(h.AddBalance, &provider)

	rt.Verify()

	rt.SetBalance(big.Add(rt.Balance(), amount))
}

// addParticipantFunds is a helper method to setup non-provider storage market participant funds
func (h *marketActorTestHarness) addParticipantFunds(rt *mock.Runtime, addr address.Address, amount abi.TokenAmount) {
	rt.SetReceived(amount)
	rt.SetCaller(addr, builtin.AccountActorCodeID)
	rt.ExpectValidateCallerType(builtin.CallerTypesSignable...)

	rt.Call(h.AddBalance, &addr)

	rt.Verify()

	rt.SetBalance(big.Add(rt.Balance(), amount))
}

func (h *marketActorTestHarness) expectProviderControlAddressesAndValidateCaller(rt *mock.Runtime, provider address.Address, owner address.Address, worker address.Address) {
	rt.ExpectValidateCallerAddr(owner, worker)

	expectRet := &miner.GetControlAddressesReturn{Owner: owner, Worker: worker}

	rt.ExpectSend(
		provider,
		builtin.MethodsMiner.ControlAddresses,
		nil,
		big.Zero(),
		expectRet,
		exitcode.Ok,
	)
}

func (h *marketActorTestHarness) withdrawStorageMinerBalanceOK(rt *mock.Runtime, owner, worker, provider address.Address, withDrawAmt, expectedSend abi.TokenAmount) {
	rt.SetCaller(worker, builtin.AccountActorCodeID)
	h.expectProviderControlAddressesAndValidateCaller(rt, provider, owner, worker)

	params := market.WithdrawBalanceParams{
		ProviderOrClientAddress: provider,
		Amount:                  withDrawAmt,
	}

	rt.ExpectSend(owner, builtin.MethodSend, nil, expectedSend, nil, exitcode.Ok)
	rt.Call(h.WithdrawBalance, &params)
	rt.Verify()
}

func (h *marketActorTestHarness) withdrawClientBalanceOK(rt *mock.Runtime, client address.Address, withDrawAmt, expectedSend abi.TokenAmount) {
	rt.SetCaller(client, builtin.AccountActorCodeID)
	rt.ExpectValidateCallerType(builtin.CallerTypesSignable...)
	rt.ExpectSend(client, builtin.MethodSend, nil, expectedSend, nil, exitcode.Ok)

	params := market.WithdrawBalanceParams{
		ProviderOrClientAddress: client,
		Amount:                  withDrawAmt,
	}

	rt.Call(h.WithdrawBalance, &params)
	rt.Verify()
}

func (h *marketActorTestHarness) publishDeal(rt *mock.Runtime, deal *market.DealProposal, owner, worker, provider address.Address) abi.DealID {
	rt.SetCaller(worker, builtin.AccountActorCodeID)
	rt.ExpectValidateCallerType(builtin.CallerTypesSignable...)
	rt.ExpectSend(
		provider,
		builtin.MethodsMiner.ControlAddresses,
		nil,
		big.Zero(),
		&miner.GetControlAddressesReturn{Owner: owner, Worker: worker},
		exitcode.Ok,
	)

	//  create a client proposal with a valid signature
	buf := bytes.Buffer{}
	require.NoError(h.t, deal.MarshalCBOR(&buf), "failed to marshal deal proposal")
	sig := crypto.Signature{Type: crypto.SigTypeBLS, Data: []byte("does not matter")}
	clientProposal := market.ClientDealProposal{*deal, sig}
	params := &market.PublishStorageDealsParams{[]market.ClientDealProposal{clientProposal}}

	// expect a call to verify the above signature
	rt.ExpectVerifySignature(sig, deal.Client, buf.Bytes(), nil)

	ret := rt.Call(h.PublishStorageDeals, params)
	rt.Verify()

	resp, ok := ret.(*market.PublishStorageDealsReturn)
	require.True(h.t, ok, "unexpected type returned from call to PublishStorageDeals")
	require.Len(h.t, resp.IDs, 1)

	dealId := resp.IDs[0]
	require.NotNil(h.t, h.mustGetDealProposal(rt, dealId))

	return dealId
}

func (h *marketActorTestHarness) generateUnVerifiedDealProposal(client, provider address.Address, startEpoch, endEpoch abi.ChainEpoch) *market.DealProposal {
	buf := make([]byte, binary.MaxVarintLen64)
	binary.PutVarint(buf, int64(rand.Int()))
	hash, err := mh.Sum(buf, mh.SHA2_256, -1)
	require.NoError(h.t, err)

	pieceCid := cid.NewCidV0(hash)
	pieceSize := abi.PaddedPieceSize(2048)
	storagePerEpoch := big.NewInt(int64(rand.Intn(1000) + 1))
	clientCollateral := big.NewInt(int64(rand.Intn(1000) + 1))
	providerCollateral := big.NewInt(int64(rand.Intn(1000) + 1))

	return &market.DealProposal{pieceCid, pieceSize, false, client, provider, startEpoch,
		endEpoch, storagePerEpoch, providerCollateral, clientCollateral}
}

func (h *marketActorTestHarness) activeDealOK(rt *mock.Runtime, dealID abi.DealID, sectorExpiry abi.ChainEpoch, minerAddr address.Address) {
	rt.SetCaller(minerAddr, builtin.StorageMinerActorCodeID)
	rt.ExpectValidateCallerType(builtin.StorageMinerActorCodeID)

	params := &market.ActivateDealsParams{DealIDs: []abi.DealID{dealID}, SectorExpiry: sectorExpiry}

	ret := rt.Call(h.ActivateDeals, params)
	rt.Verify()

	require.Nil(h.t, ret)
}

func (h *marketActorTestHarness) mustGetDealProposal(rt *mock.Runtime, dealID abi.DealID) *market.DealProposal {
	var st market.State
	rt.GetState(&st)

	proposals, err := market.AsDealProposalArray(adt.AsStore(rt), st.Proposals)
	require.NoError(h.t, err)

	d, err := proposals.Get(dealID)
	require.NoError(h.t, err)

	return d
}

func (h *marketActorTestHarness) mustGetDealState(rt *mock.Runtime, dealID abi.DealID) *market.DealState {
	var st market.State
	rt.GetState(&st)

	states, err := market.AsDealStateArray(adt.AsStore(rt), st.States)
	require.NoError(h.t, err)

	s, found, err := states.Get(dealID)
	require.NoError(h.t, err)
	require.True(h.t, found)
	require.NotNil(h.t, s)

	return s
}

func (h *marketActorTestHarness) terminateDealOK(rt *mock.Runtime, dealID abi.DealID, minerAddr address.Address) {
	rt.SetCaller(minerAddr, builtin.StorageMinerActorCodeID)
	rt.ExpectValidateCallerType(builtin.StorageMinerActorCodeID)

	params := &market.OnMinerSectorsTerminateParams{DealIDs: []abi.DealID{dealID}}

	ret := rt.Call(h.OnMinerSectorsTerminate, params)
	rt.Verify()

	require.Nil(h.t, ret)
}
