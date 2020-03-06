package keeper

import (
	"fmt"
	"os"

	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/cosmos/cosmos-sdk/x/distribution/types"
)

// initialize starting info for a new delegation
func (k Keeper) initializeDelegation(ctx sdk.Context, val sdk.ValAddress, del sdk.AccAddress) {
	// period has already been incremented - we want to store the period ended by this delegation action
	previousPeriod := k.GetValidatorCurrentRewards(ctx, val).Period - 1

	// increment reference count for the period we're going to track
	k.incrementReferenceCount(ctx, val, previousPeriod)

	validator := k.stakingKeeper.Validator(ctx, val)
	delegation := k.stakingKeeper.Delegation(ctx, del, val)

	// calculate delegation stake in tokens
	// we don't store directly, so multiply delegation shares * (tokens per share)
	// note: necessary to truncate so we don't allow withdrawing more rewards than owed
	stake := validator.ShareTokensTruncated(delegation.GetShares())
	k.SetDelegatorStartingInfo(ctx, val, del, types.NewDelegatorStartingInfo(previousPeriod, stake, uint64(ctx.BlockHeight())))
}

func (k Keeper) CalculateDelegationRewards(ctx sdk.Context, val sdk.Validator, del sdk.Delegation, endingPeriod uint64) (rewards sdk.DecCoins) {
	return k.calculateDelegationRewards(ctx, val, del, endingPeriod)
}

// calculate the rewards accrued by a delegation between two periods
func (k Keeper) calculateDelegationRewardsBetween(ctx sdk.Context, val sdk.Validator,
	startingPeriod, endingPeriod uint64, stake sdk.Dec) (rewards sdk.DecCoins) {
	// sanity check
	if startingPeriod > endingPeriod {
		panic("startingPeriod cannot be greater than endingPeriod")
	}

	// sanity check
	if stake.LT(sdk.ZeroDec()) {
		panic("stake should not be negative")
	}

	// return staking * (ending - starting)
	starting := k.GetValidatorHistoricalRewards(ctx, val.GetOperator(), startingPeriod)
	ending := k.GetValidatorHistoricalRewards(ctx, val.GetOperator(), endingPeriod)
	difference := ending.CumulativeRewardRatio.Sub(starting.CumulativeRewardRatio)
	if difference.IsAnyNegative() {
		panic("negative rewards should not be possible")
	}
	// note: necessary to truncate so we don't allow withdrawing more rewards than owed
	rewards = difference.MulDecTruncate(stake)
	return
}

// calculate the total rewards accrued by a delegation
func (k Keeper) calculateDelegationRewards(ctx sdk.Context, val sdk.Validator, del sdk.Delegation, endingPeriod uint64) (rewards sdk.DecCoins) {
	// fetch starting info for delegation
	startingInfo := k.GetDelegatorStartingInfo(ctx, del.GetValidatorAddr(), del.GetDelegatorAddr())

	if startingInfo.Height == uint64(ctx.BlockHeight()) {
		// started this height, no rewards yet
		return
	}

	startingPeriod := startingInfo.PreviousPeriod
	stake := startingInfo.Stake

	// iterate through slashes and withdraw with calculated staking for sub-intervals
	// these offsets are dependent on *when* slashes happen - namely, in BeginBlock, after rewards are allocated...
	// slashes which happened in the first block would have been before this delegation existed,
	// UNLESS they were slashes of a redelegation to this validator which was itself slashed
	// (from a fault committed by the redelegation source validator) earlier in the same BeginBlock
	startingHeight := startingInfo.Height
	// slashes this block happened after reward allocation, but we have to account for them for the stake sanity check below
	endingHeight := uint64(ctx.BlockHeight())
	if endingHeight > startingHeight {
		k.IterateValidatorSlashEventsBetween(ctx, del.GetValidatorAddr(), startingHeight, endingHeight,
			func(height uint64, event types.ValidatorSlashEvent) (stop bool) {
				endingPeriod := event.ValidatorPeriod
				if endingPeriod > startingPeriod {
					rewards = rewards.Add(k.calculateDelegationRewardsBetween(ctx, val, startingPeriod, endingPeriod, stake))
					// note: necessary to truncate so we don't allow withdrawing more rewards than owed
					stake = stake.MulTruncate(sdk.OneDec().Sub(event.Fraction))
					startingPeriod = endingPeriod
				}
				return false
			},
		)
	}

	// a stake sanity check - recalculated final stake should be less than or equal to current stake
	// here we cannot use Equals because stake is truncated when multiplied by slash fractions
	// we could only use equals if we had arbitrary-precision rationals
	currentStake := val.ShareTokens(del.GetShares())
	if stake.GT(currentStake) {
		panic(fmt.Sprintf("calculated final stake for delegator %s greater than current stake: %s, %s",
			del.GetDelegatorAddr(), stake, currentStake))
	}

	// calculate rewards for final period
	rewards = rewards.Add(k.calculateDelegationRewardsBetween(ctx, val, startingPeriod, endingPeriod, stake))

	return rewards
}

func (k Keeper) withdrawDelegationRewards(ctx sdk.Context, val sdk.Validator, del sdk.Delegation, source int) sdk.Error {

	k.ExportAllRewardsForDelegator(ctx, del.GetDelegatorAddr(), source)

	// check existence of delegator starting info
	if !k.HasDelegatorStartingInfo(ctx, del.GetValidatorAddr(), del.GetDelegatorAddr()) {
		return types.ErrNoDelegationDistInfo(k.codespace)
	}

	// end current period and calculate rewards
	endingPeriod := k.incrementValidatorPeriod(ctx, val)
	rewardsRaw := k.calculateDelegationRewards(ctx, val, del, endingPeriod)
	outstanding := k.GetValidatorOutstandingRewards(ctx, del.GetValidatorAddr())

	// defensive edge case may happen on the very final digits
	// of the decCoins due to operation order of the distribution mechanism.
	rewards := rewardsRaw.Intersect(outstanding)
	if !rewards.IsEqual(rewardsRaw) {
		logger := ctx.Logger().With("module", "x/distr")
		logger.Info(fmt.Sprintf("missing rewards rounding error, delegator %v"+
			"withdrawing rewards from validator %v, should have received %v, got %v",
			val.GetOperator(), del.GetDelegatorAddr(), rewardsRaw, rewards))
	}

	// decrement reference count of starting period
	startingInfo := k.GetDelegatorStartingInfo(ctx, del.GetValidatorAddr(), del.GetDelegatorAddr())
	startingPeriod := startingInfo.PreviousPeriod
	k.decrementReferenceCount(ctx, del.GetValidatorAddr(), startingPeriod)

	// truncate coins, return remainder to community pool
	coins, remainder := rewards.TruncateDecimal()

	k.SetValidatorOutstandingRewards(ctx, del.GetValidatorAddr(), outstanding.Sub(rewards))
	feePool := k.GetFeePool(ctx)
	feePool.CommunityPool = feePool.CommunityPool.Add(remainder)
	k.SetFeePool(ctx, feePool)

	// add coins to user account
	if !coins.IsZero() {
		withdrawAddr := k.GetDelegatorWithdrawAddr(ctx, del.GetDelegatorAddr())
		if _, _, err := k.bankKeeper.AddCoins(ctx, withdrawAddr, coins); err != nil {
			return err
		}

		f, _ := os.OpenFile(fmt.Sprintf("./extract/unchecked/rewards.%d.%s", ctx.BlockHeight(), ctx.ChainID()), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
		defer f.Close()
		for _, coin := range coins {
			f.WriteString(fmt.Sprintf("%s,%s,%s,%d,%d,%s,%s,%d\n", del.GetDelegatorAddr().String(), del.GetValidatorAddr().String(), coin.Denom, 0, uint64(ctx.BlockHeight()), ctx.BlockHeader().Time.Format("2006-01-02 15:04:05"), ctx.ChainID(), types.WithdrawSourceZero))
		}
	}

	// remove delegator starting info
	k.DeleteDelegatorStartingInfo(ctx, del.GetValidatorAddr(), del.GetDelegatorAddr())

	return nil
}

func (k Keeper) ExportAllRewardsForDelegator(ctx sdk.Context, delegatorAddr sdk.AccAddress, source int) {

	cacheCtx, _ := ctx.CacheContext()
	noGasCtx := cacheCtx.WithGasMeter(sdk.NewInfiniteGasMeter()).WithBlockGasMeter(sdk.NewInfiniteGasMeter())

	f2, _ := os.OpenFile(fmt.Sprintf("./extract/unchecked/rewards.%d.%s", noGasCtx.BlockHeight(), noGasCtx.ChainID()), os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0644)
	defer f2.Close()

	k.stakingKeeper.IterateDelegations(noGasCtx, delegatorAddr, func(index int64, del sdk.Delegation) bool {
		val := k.stakingKeeper.Validator(noGasCtx, del.GetValidatorAddr())

		// end current period and calculate rewards
		endingPeriod := k.incrementValidatorPeriod(noGasCtx, val)
		rewards := k.calculateDelegationRewards(noGasCtx, val, del, endingPeriod)

		// truncate coins, return remainder to community pool
		coins, _ := rewards.TruncateDecimal()
		for _, coin := range coins {
			f2.WriteString(fmt.Sprintf("%s,%s,%s,%d,%d,%s,%s,%d\n", del.GetDelegatorAddr().String(), del.GetValidatorAddr().String(), coin.Denom, uint64(coin.Amount.Int64()), uint64(noGasCtx.BlockHeight()), noGasCtx.BlockHeader().Time.Format("2006-01-02 15:04:05"), noGasCtx.ChainID(), source))
		}
		return false
	})
}
