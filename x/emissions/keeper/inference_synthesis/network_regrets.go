package inference_synthesis

import (
	"sort"

	errorsmod "cosmossdk.io/errors"
	alloraMath "github.com/allora-network/allora-chain/math"
	"github.com/allora-network/allora-chain/x/emissions/keeper"
	emissions "github.com/allora-network/allora-chain/x/emissions/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
)

type networkLossesByWorker struct {
	CombinedLoss           Loss
	InfererLosses          map[Worker]Loss
	ForecasterLosses       map[Worker]Loss
	NaiveLoss              Loss
	OneOutInfererLosses    map[Worker]Loss
	OneOutForecasterLosses map[Worker]Loss
	OneInForecasterLosses  map[Worker]Loss
}

// Convert a ValueBundle to a networkLossesByWorker
func ConvertValueBundleToNetworkLossesByWorker(
	valueBundle emissions.ValueBundle,
) networkLossesByWorker {
	infererLosses := make(map[Worker]Loss)
	for _, inferer := range valueBundle.InfererValues {
		infererLosses[inferer.Worker] = inferer.Value
	}

	forecasterLosses := make(map[Worker]Loss)
	for _, forecaster := range valueBundle.ForecasterValues {
		forecasterLosses[forecaster.Worker] = forecaster.Value
	}

	oneOutInfererLosses := make(map[Worker]Loss)
	for _, oneOutInferer := range valueBundle.OneOutInfererValues {
		oneOutInfererLosses[oneOutInferer.Worker] = oneOutInferer.Value
	}

	oneOutForecasterLosses := make(map[Worker]Loss)
	for _, oneOutForecaster := range valueBundle.OneOutForecasterValues {
		oneOutForecasterLosses[oneOutForecaster.Worker] = oneOutForecaster.Value
	}

	oneInForecasterLosses := make(map[Worker]Loss)
	for _, oneInForecaster := range valueBundle.OneInForecasterValues {
		oneInForecasterLosses[oneInForecaster.Worker] = oneInForecaster.Value
	}

	return networkLossesByWorker{
		CombinedLoss:           valueBundle.CombinedValue,
		InfererLosses:          infererLosses,
		ForecasterLosses:       forecasterLosses,
		NaiveLoss:              valueBundle.NaiveValue,
		OneOutInfererLosses:    oneOutInfererLosses,
		OneOutForecasterLosses: oneOutForecasterLosses,
		OneInForecasterLosses:  oneInForecasterLosses,
	}
}

func ComputeAndBuildEMRegret(
	lossA Loss,
	lossB Loss,
	previousRegret Regret,
	alpha alloraMath.Dec,
	blockHeight BlockHeight,
) (emissions.TimestampedValue, error) {
	lossDiff, err := lossA.Sub(lossB)
	if err != nil {
		return emissions.TimestampedValue{}, err
	}

	newRegret, err := alloraMath.CalcEma(alpha, lossDiff, previousRegret, false)
	if err != nil {
		return emissions.TimestampedValue{}, err
	}
	return emissions.TimestampedValue{
		BlockHeight: blockHeight,
		Value:       newRegret,
	}, nil
}

// Calculate the new network regrets by taking EMAs between the previous network regrets
// and the new regrets admitted by the inputted network losses
// It is assumed the workers are uniquely represented in the network losses
func GetCalcSetNetworkRegrets(
	ctx sdk.Context,
	k keeper.Keeper,
	topicId TopicId,
	networkLosses emissions.ValueBundle,
	nonce emissions.Nonce,
	alpha alloraMath.Dec,
	cNorm alloraMath.Dec,
	pNorm alloraMath.Dec,
	epsilon alloraMath.Dec,
) error {
	// Convert the network losses to a networkLossesByWorker
	networkLossesByWorker := ConvertValueBundleToNetworkLossesByWorker(networkLosses)
	blockHeight := nonce.BlockHeight

	workersRegrets := make([]alloraMath.Dec, 0)

	// Get old regret R_{i-1,j} and Calculate then Set the new regrets R_ij for inferers
	sort.Slice(networkLosses.InfererValues, func(i, j int) bool {
		return networkLosses.InfererValues[i].Worker < networkLosses.InfererValues[j].Worker
	})
	for _, infererLoss := range networkLosses.InfererValues {
		lastRegret, newParticipant, err := k.GetInfererNetworkRegret(ctx, topicId, infererLoss.Worker)
		if err != nil {
			return errorsmod.Wrapf(err, "failed to get inferer regret")
		}
		newInfererRegret, err := ComputeAndBuildEMRegret(
			networkLosses.CombinedValue,
			networkLossesByWorker.InfererLosses[infererLoss.Worker],
			lastRegret.Value,
			alpha,
			blockHeight,
		)
		if err != nil {
			return errorsmod.Wrapf(err, "Error computing and building inferer regret")
		}
		k.SetInfererNetworkRegret(ctx, topicId, infererLoss.Worker, newInfererRegret)
		if !newParticipant {
			workersRegrets = append(workersRegrets, newInfererRegret.Value)
		}
	}

	// Get old regret R_{i-1,k} and Calculate then Set the new regrets R_ik for forecasters
	sort.Slice(networkLosses.ForecasterValues, func(i, j int) bool {
		return networkLosses.ForecasterValues[i].Worker < networkLosses.ForecasterValues[j].Worker
	})
	for _, forecasterLoss := range networkLosses.ForecasterValues {
		lastRegret, newParticipant, err := k.GetForecasterNetworkRegret(ctx, topicId, forecasterLoss.Worker)
		if err != nil {
			return errorsmod.Wrapf(err, "Error getting forecaster regret")
		}
		newForecasterRegret, err := ComputeAndBuildEMRegret(
			networkLosses.CombinedValue,
			networkLossesByWorker.ForecasterLosses[forecasterLoss.Worker],
			lastRegret.Value,
			alpha,
			blockHeight,
		)
		if err != nil {
			return errorsmod.Wrapf(err, "Error computing and building forecaster regret")
		}
		k.SetForecasterNetworkRegret(ctx, topicId, forecasterLoss.Worker, newForecasterRegret)
		if !newParticipant {
			workersRegrets = append(workersRegrets, newForecasterRegret.Value)
		}
	}

	// Calculate the new one-in regrets for the forecasters R^+_ij'k where j' includes all j and forecast implied inference from forecaster k
	sort.Slice(networkLosses.OneInForecasterValues, func(i, j int) bool {
		return networkLosses.OneInForecasterValues[i].Worker < networkLosses.OneInForecasterValues[j].Worker
	})
	for _, oneInForecasterLoss := range networkLosses.OneInForecasterValues {
		// Loop over the inferer losses so that their losses may be compared against the one-in forecaster's loss, for each forecaster
		for _, infererLoss := range networkLosses.InfererValues {
			lastRegret, _, err := k.GetOneInForecasterNetworkRegret(ctx, topicId, oneInForecasterLoss.Worker, infererLoss.Worker)
			if err != nil {
				return errorsmod.Wrapf(err, "Error getting one-in forecaster regret")
			}
			newOneInForecasterRegret, err := ComputeAndBuildEMRegret(
				networkLossesByWorker.OneInForecasterLosses[oneInForecasterLoss.Worker],
				networkLossesByWorker.InfererLosses[infererLoss.Worker],
				lastRegret.Value,
				alpha,
				blockHeight,
			)
			if err != nil {
				return errorsmod.Wrapf(err, "Error computing and building one-in forecaster regret")
			}
			k.SetOneInForecasterNetworkRegret(ctx, topicId, oneInForecasterLoss.Worker, infererLoss.Worker, newOneInForecasterRegret)
		}
		// Self-regret for the forecaster given their own regret
		lastRegret, _, err := k.GetOneInForecasterSelfNetworkRegret(ctx, topicId, oneInForecasterLoss.Worker)
		if err != nil {
			return errorsmod.Wrapf(err, "Error getting one-in forecaster self regret")
		}
		oneInForecasterSelfRegret, err := ComputeAndBuildEMRegret(
			networkLossesByWorker.OneInForecasterLosses[oneInForecasterLoss.Worker],
			networkLossesByWorker.ForecasterLosses[oneInForecasterLoss.Worker],
			lastRegret.Value,
			alpha,
			blockHeight,
		)
		if err != nil {
			return errorsmod.Wrapf(err, "Error computing and building one-in forecaster self regret")
		}
		k.SetOneInForecasterSelfNetworkRegret(ctx, topicId, oneInForecasterLoss.Worker, oneInForecasterSelfRegret)
	}

	// Recalculate topic initial regret
	if len(workersRegrets) > 0 {
		updatedTopicInitialRegret, err := CalcTopicInitialRegret(workersRegrets, epsilon, pNorm, cNorm)
		if err != nil {
			return errorsmod.Wrapf(err, "Error calculating topic initial regret")
		}
		k.UpdateTopicInitialRegret(ctx, topicId, updatedTopicInitialRegret)
	}

	return nil
}

func CalcTopicInitialRegret(regrets []alloraMath.Dec, epsilon alloraMath.Dec, pNorm alloraMath.Dec, cNorm alloraMath.Dec) (alloraMath.Dec, error) {
	// Calculate the Denominator
	stdDevRegrets, err := alloraMath.StdDev(regrets)
	if err != nil {
		return alloraMath.ZeroDec(), err
	}

	denominator, err := stdDevRegrets.Add(epsilon)
	if err != nil {
		return alloraMath.ZeroDec(), err
	}

	// calculate the offset
	eightPointTwoFive := alloraMath.MustNewDecFromString("8.25")

	eightPointTwoFiveDividedByPnorm, err := eightPointTwoFive.Quo(pNorm)
	if err != nil {
		return alloraMath.ZeroDec(), err
	}

	offset, err := cNorm.Sub(eightPointTwoFiveDividedByPnorm)
	if err != nil {
		return alloraMath.ZeroDec(), err
	}

	// calculate the dummy regret
	offSetTimesDenominator, err := offset.Mul(denominator)
	if err != nil {
		return alloraMath.ZeroDec(), err
	}

	minimumRegret := alloraMath.ZeroDec()
	for i, regret := range regrets {
		if i == 0 || regret.Lt(minimumRegret) {
			minimumRegret = regret
		}
	}

	dummyRegret, err := minimumRegret.Add(offSetTimesDenominator)
	if err != nil {
		return alloraMath.ZeroDec(), err
	}

	return dummyRegret, nil
}
