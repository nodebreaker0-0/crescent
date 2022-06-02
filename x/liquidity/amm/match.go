package amm

import (
	"fmt"
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

	prec := TickPrecision(tickPrec)
	lowestTickIdx := prec.TickToIndex(prec.LowestTick())
	highestTickIdx := prec.TickToIndex(prec.HighestTick())
	var i, j int
	i, found = findFirstTrueCondition(lowestTickIdx, highestTickIdx, func(i int) bool {
		return os.BuyAmountOver(prec.TickFromIndex(i + 1)).LTE(os.SellAmountUnder(prec.TickFromIndex(i)))
	})
	if !found {
		return sdk.Dec{}, false
	}
	j, found = findFirstTrueCondition(highestTickIdx, lowestTickIdx, func(i int) bool {
		return os.BuyAmountOver(prec.TickFromIndex(i)).GTE(os.SellAmountUnder(prec.TickFromIndex(i - 1)))
	})
	if !found {
		return sdk.Dec{}, false
	}
	midTick := TickFromIndex(i, tickPrec).Add(TickFromIndex(j, tickPrec)).QuoInt64(2)
	return RoundPrice(midTick, tickPrec), true
}

// findFirstTrueCondition uses the binary search to find the first index
// where f(i) is true, while searching in range [start, end].
// It assumes that f(j) == false where j < i and f(j) == true where j >= i.
// start can be greater than end.
func findFirstTrueCondition(start, end int, f func(i int) bool) (i int, found bool) {
	if start < end {
		i = start + sort.Search(end-start+1, func(i int) bool {
			return f(start + i)
		})
		if i > end {
			return 0, false
		}
		return i, true
	}
	i = start - sort.Search(start-end+1, func(i int) bool {
		return f(start - i)
	})
	if i < end {
		return 0, false
	}
	return i, true
}

func (ob *OrderBook) InstantMatch(ctx MatchContext, lastPrice sdk.Dec) (matched bool) {
	buyTicks := make([]*orderBookTick, 0, len(ob.buys.ticks))
	buySums := make([]sdk.Int, 0, len(ob.buys.ticks))
	for i, buyTick := range ob.buys.ticks {
		if buyTick.price.LT(lastPrice) {
			break
		}
		sum := ctx.TotalOpenAmount(buyTick.orders())
		if i > 0 {
			sum = buySums[i-1].Add(sum)
		}
		buyTicks = append(buyTicks, buyTick)
		buySums = append(buySums, sum)
	}
	sellTicks := make([]*orderBookTick, 0, len(ob.sells.ticks))
	sellSums := make([]sdk.Int, 0, len(ob.sells.ticks))
	for i, sellTick := range ob.sells.ticks {
		if sellTick.price.GT(lastPrice) {
			break
		}
		sum := ctx.TotalOpenAmount(sellTick.orders())
		if i > 0 {
			sum = sellSums[i-1].Add(sum)
		}
		sellTicks = append(sellTicks, sellTick)
		sellSums = append(sellSums, sum)
	}
	if len(buyTicks) == 0 || len(sellTicks) == 0 {
		return false
	}
	matchAmt := sdk.MinInt(buySums[len(buySums)-1], sellSums[len(sellSums)-1])
	bi := sort.Search(len(buySums), func(i int) bool {
		return buySums[i].GTE(matchAmt)
	})
	si := sort.Search(len(sellSums), func(i int) bool {
		return sellSums[i].GTE(matchAmt)
	})
	distributeAmtToTicks := func(ticks []*orderBookTick, sums []sdk.Int, lastIdx int) {
		for _, tick := range ticks[:lastIdx] {
			ctx.MatchOrdersFull(tick.orders(), lastPrice)
		}
		var remainingAmt sdk.Int
		if lastIdx == 0 {
			remainingAmt = matchAmt
		} else {
			remainingAmt = matchAmt.Sub(sums[lastIdx-1])
		}
		lastOrders := ticks[lastIdx].orders()
		if ctx.TotalOpenAmount(lastOrders).Equal(remainingAmt) {
			ctx.MatchOrdersFull(lastOrders, lastPrice)
		} else {
			DistributeOrderAmount(ctx, lastOrders, lastPrice, remainingAmt)
		}
	}
	distributeAmtToTicks(buyTicks, buySums, bi)
	distributeAmtToTicks(sellTicks, sellSums, si)
	return true
}

// DistributeOrderAmount distributes the given order amount to the orders
// proportional to each order's amount.
// After distributing the amount based on each order's proportion,
// remaining amount due to the decimal truncation is distributed
// to the orders again, by priority.
// This time, the proportion is not considered and each order takes up
// the amount as much as possible.
func DistributeOrderAmount(ctx MatchContext, orders []Order, matchPrice sdk.Dec, amt sdk.Int) {
	totalAmt := TotalAmount(orders)
	totalMatchedAmt := sdk.ZeroInt()
	matchedAmtByOrder := map[Order]sdk.Int{}

	for _, order := range orders {
		openAmt := ctx.OpenAmount(order)
		if openAmt.IsZero() {
			continue
		}
		orderAmt := order.GetAmount().ToDec()
		proportion := orderAmt.QuoTruncate(totalAmt.ToDec())
		matchedAmt := sdk.MinInt(openAmt, proportion.MulInt(amt).TruncateInt())
		if matchedAmt.IsPositive() {
			matchedAmtByOrder[order] = matchedAmt
			totalMatchedAmt = totalMatchedAmt.Add(matchedAmt)
		}
	}

	remainingAmt := amt.Sub(totalMatchedAmt)
	for _, order := range orders {
		if remainingAmt.IsZero() {
			break
		}
		openAmt := ctx.OpenAmount(order)
		matchedAmt := sdk.MinInt(remainingAmt, sdk.MinInt(openAmt, order.GetAmount()))
		prevMatchedAmt, ok := matchedAmtByOrder[order]
		if !ok { // TODO: is it possible?
			prevMatchedAmt = sdk.ZeroInt()
		}
		matchedAmtByOrder[order] = prevMatchedAmt.Add(matchedAmt)
		remainingAmt = remainingAmt.Sub(matchedAmt)
	}

	var matchedOrders, notMatchedOrders []Order
	for _, order := range orders {
		matchedAmt, ok := matchedAmtByOrder[order]
		if !ok {
			matchedAmt = sdk.ZeroInt()
		}
		if !matchedAmt.IsZero() && (order.GetDirection() == Buy || matchPrice.MulInt(matchedAmt).TruncateInt().IsPositive()) {
			matchedOrders = append(matchedOrders, order)
		} else {
			notMatchedOrders = append(notMatchedOrders, order)
		}
	}

	if len(notMatchedOrders) > 0 {
		DistributeOrderAmount(ctx, matchedOrders, matchPrice, amt)
		return
	}

	for order, matchedAmt := range matchedAmtByOrder {
		ctx.MatchOrder(order, matchedAmt, matchPrice)
	}
}

type MatchRecord struct {
	Amount sdk.Int
	Price  sdk.Dec
}

type MatchResult struct {
	OpenAmount   sdk.Int
	MatchRecords []MatchRecord
}

type MatchContext map[Order]*MatchResult

func NewMatchContext() MatchContext {
	return MatchContext{}
}

func (ctx MatchContext) MatchOrder(order Order, amt sdk.Int, price sdk.Dec) {
	if openAmt := ctx.OpenAmount(order); amt.GT(openAmt) {
		panic(fmt.Errorf("cannot match more than open amount; %s > %s", amt, openAmt))
	}
	mr, ok := ctx[order]
	if !ok {
		mr = &MatchResult{
			OpenAmount: order.GetAmount(),
		}
		ctx[order] = mr
	}
	mr.OpenAmount = mr.OpenAmount.Sub(amt)
	mr.MatchRecords = append(mr.MatchRecords, MatchRecord{
		Amount: amt,
		Price:  price,
	})
}

func (ctx MatchContext) MatchOrderFull(order Order, price sdk.Dec) {
	openAmt := ctx.OpenAmount(order)
	if openAmt.IsPositive() {
		ctx.MatchOrder(order, ctx.OpenAmount(order), price)
	}
}

func (ctx MatchContext) MatchOrdersFull(orders []Order, price sdk.Dec) {
	for _, order := range orders {
		ctx.MatchOrderFull(order, price)
	}
}

func (ctx MatchContext) OpenAmount(order Order) sdk.Int {
	mr, ok := ctx[order]
	if !ok {
		return order.GetAmount()
	}
	return mr.OpenAmount
}

func (ctx MatchContext) TotalOpenAmount(orders []Order) sdk.Int {
	amt := sdk.ZeroInt()
	for _, order := range orders {
		amt = amt.Add(ctx.OpenAmount(order))
	}
	return amt
}

func (ctx MatchContext) MatchedAmount(order Order) sdk.Int {
	mr, ok := ctx[order]
	if !ok {
		return sdk.ZeroInt()
	}
	return order.GetAmount().Sub(mr.OpenAmount)
}

// SortOrdersByBatchId returns orders grouped and sorted by batch id.
func SortOrdersByBatchId(orders []Order) [][]Order {
	ordersByBatchId := map[uint64][]Order{}
	for _, order := range orders {
		ordersByBatchId[order.GetBatchId()] = append(ordersByBatchId[order.GetBatchId()], order)
	}
	batchIds := make([]uint64, 0, len(ordersByBatchId))
	for batchId := range ordersByBatchId {
		batchIds = append(batchIds, batchId)
	}
	sort.Slice(batchIds, func(i, j int) bool {
		if batchIds[i] == 0 {
			return false
		}
		if batchIds[j] == 0 {
			return true
		}
		return batchIds[i] < batchIds[j]
	})
	groups := make([][]Order, 0, len(batchIds))
	for _, batchId := range batchIds {
		groups = append(groups, ordersByBatchId[batchId])
	}
	return groups
}
