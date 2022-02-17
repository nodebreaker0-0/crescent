package amm

import (
	"math"
	"sort"

	sdk "github.com/cosmos/cosmos-sdk/types"
)

// FindMatchPrice returns the best match price for given order sources.
// If there is no matchable orders, found will be false.
func FindMatchPrice(os OrderSource, tickPrec int) (matchPrice sdk.Dec, found bool) {
	highestBuyPrice, found := os.HighestBuyPrice()
	if !found {
		return sdk.Dec{}, false
	}
	lowestSellPrice, found := os.LowestSellPrice()
	if !found {
		return sdk.Dec{}, false
	}
	if highestBuyPrice.LT(lowestSellPrice) {
		return sdk.Dec{}, false
	}

	lowestTickIdx := TickToIndex(LowestTick(tickPrec), tickPrec)
	highestTickIdx := TickToIndex(HighestTick(tickPrec), tickPrec)
	i := findFirstTrueCondition(lowestTickIdx, highestTickIdx, func(i int) bool {
		return os.BuyAmountOver(TickFromIndex(i+1, tickPrec)).LTE(os.SellAmountUnder(TickFromIndex(i, tickPrec)))
	})
	j := findFirstTrueCondition(highestTickIdx, lowestTickIdx, func(i int) bool {
		return os.BuyAmountOver(TickFromIndex(i, tickPrec)).GTE(os.SellAmountUnder(TickFromIndex(i-1, tickPrec)))
	})
	if i == j {
		return TickFromIndex(i, tickPrec), true
	}
	if int(math.Abs(float64(i-j))) > 1 { // sanity check
		panic("impossible case")
	}
	return TickFromIndex(RoundTickIndex(i), tickPrec), true
}

// findFirstTrueCondition uses the binary search to find the first index
// where f(i) is true, while searching in range [start, end].
// It assumes that f(j) == false where j < i and f(j) == true where j >= i.
// start can be greater than end.
func findFirstTrueCondition(start, end int, f func(i int) bool) (i int) {
	if start < end {
		i = start + sort.Search(end-start+1, func(i int) bool {
			return f(start + i)
		})
		if i > end {
			panic("impossible case")
		}
	} else {
		i = start - sort.Search(start-end+1, func(i int) bool {
			return f(start - i)
		})
		if i < end {
			panic("impossible case")
		}
	}
	return
}

// FindLastMatchableOrders returns the last matchable order indexes for
// each buy/sell side.
// lastBuyPartialMatchAmt and lastSellPartialMatchAmt are
// the amount of partially matched portion of the last orders.
// FindLastMatchableOrders drops(ignores) an order if the orderer
// receives zero demand coin after truncation when the order is either
// fully matched or partially matched.
func FindLastMatchableOrders(buyOrders, sellOrders []Order, matchPrice sdk.Dec) (lastBuyIdx, lastSellIdx int, lastBuyPartialMatchAmt, lastSellPartialMatchAmt sdk.Int, found bool) {
	if len(buyOrders) == 0 || len(sellOrders) == 0 {
		return 0, 0, sdk.Int{}, sdk.Int{}, false
	}
	type Side struct {
		orders          []Order
		totalOpenAmt    sdk.Int
		i               int
		partialMatchAmt sdk.Int
	}
	buySide := &Side{buyOrders, TotalOpenAmount(buyOrders), len(buyOrders) - 1, sdk.Int{}}
	sellSide := &Side{sellOrders, TotalOpenAmount(sellOrders), len(sellOrders) - 1, sdk.Int{}}
	sides := map[OrderDirection]*Side{
		Buy:  buySide,
		Sell: sellSide,
	}
	// Repeatedly check both buy/sell side to see if there is an order to drop.
	// If there is not, then the loop is finished.
	for {
		ok := true
		for dir, side := range sides {
			i := side.i
			order := side.orders[i]
			matchAmt := sdk.MinInt(buySide.totalOpenAmt, sellSide.totalOpenAmt)
			otherOrdersAmt := side.totalOpenAmt.Sub(order.GetOpenAmount())
			// side.partialMatchAmt can be negative at this moment, but
			// FindLastMatchableOrders won't return a negative amount because
			// the if-block below would set ok = false if otherOrdersAmt >= matchAmt
			// and the loop would be continued.
			side.partialMatchAmt = matchAmt.Sub(otherOrdersAmt)
			if otherOrdersAmt.GTE(matchAmt) ||
				(dir == Sell && matchPrice.MulInt(side.partialMatchAmt).TruncateInt().IsZero()) {
				if i == 0 { // There's no orders left, which means orders are not matchable.
					return 0, 0, sdk.Int{}, sdk.Int{}, false
				}
				side.totalOpenAmt = side.totalOpenAmt.Sub(order.GetOpenAmount())
				side.i--
				ok = false
			}
		}
		if ok {
			return buySide.i, sellSide.i, buySide.partialMatchAmt, sellSide.partialMatchAmt, true
		}
	}
}

// MatchOrders matches orders at given matchPrice if matchable.
// Note that MatchOrders modifies the orders in the parameters.
// quoteCoinDust is the difference between total paid quote coin and total
// received quote coin.
// quoteCoinDust can be positive because of the decimal truncation.
func MatchOrders(buyOrders, sellOrders []Order, matchPrice sdk.Dec) (quoteCoinDust sdk.Int, matched bool) {
	bi, si, pmb, pms, found := FindLastMatchableOrders(buyOrders, sellOrders, matchPrice)
	if !found {
		return sdk.Int{}, false
	}

	quoteCoinDust = sdk.ZeroInt()

	for i := 0; i <= bi; i++ {
		buyOrder := buyOrders[i]
		var receivedBaseCoinAmt sdk.Int
		if i < bi {
			receivedBaseCoinAmt = buyOrder.GetOpenAmount()
		} else {
			receivedBaseCoinAmt = pmb
		}
		paidQuoteCoinAmt := matchPrice.MulInt(receivedBaseCoinAmt).Ceil().TruncateInt()
		buyOrder.SetOpenAmount(buyOrder.GetOpenAmount().Sub(receivedBaseCoinAmt))
		buyOrder.DecrRemainingOfferCoin(paidQuoteCoinAmt)
		buyOrder.IncrReceivedDemandCoin(receivedBaseCoinAmt)
		buyOrder.SetMatched(true)
		quoteCoinDust = quoteCoinDust.Add(paidQuoteCoinAmt)
	}

	for i := 0; i <= si; i++ {
		sellOrder := sellOrders[i]
		var paidBaseCoinAmt sdk.Int
		if i < si {
			paidBaseCoinAmt = sellOrder.GetOpenAmount()
		} else {
			paidBaseCoinAmt = pms
		}
		receivedQuoteCoinAmt := matchPrice.MulInt(paidBaseCoinAmt).TruncateInt()
		sellOrder.SetOpenAmount(sellOrder.GetOpenAmount().Sub(paidBaseCoinAmt))
		sellOrder.DecrRemainingOfferCoin(paidBaseCoinAmt)
		sellOrder.IncrReceivedDemandCoin(receivedQuoteCoinAmt)
		sellOrder.SetMatched(true)
		quoteCoinDust = quoteCoinDust.Sub(receivedQuoteCoinAmt)
	}

	return quoteCoinDust, true
}