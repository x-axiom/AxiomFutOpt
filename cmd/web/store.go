package main

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type MarketStore struct {
	dataDir       string
	spotCSV       string
	optionsDBPath string

	mu             sync.Mutex
	contractsReady bool
	contracts      map[string]ContractInfo
	historyCache   map[string][]DailyRecord
	backtestData   *BacktestData
	optionsDB      *sql.DB
}

type continuousPosition struct {
	CallContract string
	PutContract  string
	EntryDate    time.Time
	EntrySpot    float64
	Expiry       time.Time
	EntryValue   float64
	LastValue    float64
	LastDTE      int
}

type optionQuote struct {
	ContractCode string
	OptionType   string
	Strike       float64
	Close        float64
	Expiry       time.Time
	DaysToExpiry int
}

func (store *MarketStore) Contracts() ([]ContractInfo, error) {
	store.mu.Lock()
	defer store.mu.Unlock()

	if !store.contractsReady {
		if err := store.buildContractIndexLocked(); err != nil {
			return nil, err
		}
	}

	contracts := make([]ContractInfo, 0, len(store.contracts))
	for _, contract := range store.contracts {
		contracts = append(contracts, contract)
	}
	sort.Slice(contracts, func(i, j int) bool {
		if contracts[i].Product != contracts[j].Product {
			return contracts[i].Product < contracts[j].Product
		}
		return contracts[i].Code < contracts[j].Code
	})
	return contracts, nil
}

func (store *MarketStore) History(code string) ([]DailyRecord, error) {
	store.mu.Lock()
	defer store.mu.Unlock()

	if records, ok := store.historyCache[code]; ok {
		return append([]DailyRecord(nil), records...), nil
	}

	files, err := dataFiles(store.dataDir)
	if err != nil {
		return nil, err
	}
	records := make([]DailyRecord, 0, 256)
	for _, file := range files {
		if err := readMarketFile(file, func(record DailyRecord) {
			if record.Code == code {
				records = append(records, record)
			}
		}); err != nil {
			return nil, err
		}
	}
	if len(records) == 0 {
		return nil, fmt.Errorf("contract not found: %s", code)
	}
	sort.Slice(records, func(i, j int) bool { return records[i].Date < records[j].Date })
	store.historyCache[code] = append([]DailyRecord(nil), records...)
	return records, nil
}

func (store *MarketStore) BacktestData() (*BacktestData, error) {
	store.mu.Lock()
	defer store.mu.Unlock()

	if store.backtestData != nil {
		return store.backtestData, nil
	}

	spots, err := loadSpotClose(store.spotCSV)
	if err != nil {
		return nil, err
	}
	futures, tradingDays, err := loadIMFutures(store.dataDir)
	if err != nil {
		return nil, err
	}
	adjustExpiries(futures, tradingDays)
	store.backtestData = &BacktestData{SpotClose: spots, Futures: futures}
	return store.backtestData, nil
}

func (store *MarketStore) EligibleOptionContracts(start, end time.Time) ([]OptionContractChoice, error) {
	db, err := store.optionsDatabase()
	if err != nil {
		return nil, err
	}

	rows, err := db.Query(`
		SELECT contract_code, min(trade_date) AS first_date, max(trade_date) AS last_date
		FROM options_daily
		GROUP BY contract_code
		HAVING min(trade_date) <= ? AND max(trade_date) >= ?
		ORDER BY contract_code
	`, compactDay(start), compactDay(end))
	if err != nil {
		return nil, fmt.Errorf("query eligible option contracts: %w", err)
	}
	defer rows.Close()

	contracts := make([]OptionContractChoice, 0, 256)
	for rows.Next() {
		var contractCode string
		var firstDate string
		var lastDate string
		if err := rows.Scan(&contractCode, &firstDate, &lastDate); err != nil {
			return nil, fmt.Errorf("scan eligible option contract: %w", err)
		}
		contract, ok := optionContractChoiceFromCode(contractCode)
		if !ok {
			continue
		}
		contract.FirstDate = displayCompactDay(firstDate)
		contract.LastDate = displayCompactDay(lastDate)
		contracts = append(contracts, contract)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate eligible option contracts: %w", err)
	}
	return contracts, nil
}

func (store *MarketStore) RunStraddleBacktest(start, end time.Time, callContract, putContract string, callQty, putQty int) (StraddleBacktestResult, error) {
	callChoice, ok := optionContractChoiceFromCode(callContract)
	if !ok || callChoice.OptionType != "C" {
		return StraddleBacktestResult{}, fmt.Errorf("invalid CALL contract: %s", callContract)
	}
	putChoice, ok := optionContractChoiceFromCode(putContract)
	if !ok || putChoice.OptionType != "P" {
		return StraddleBacktestResult{}, fmt.Errorf("invalid PUT contract: %s", putContract)
	}

	db, err := store.optionsDatabase()
	if err != nil {
		return StraddleBacktestResult{}, err
	}

	rows, err := db.Query(`
		SELECT c.trade_date, c.close, p.close
		FROM options_daily c
		JOIN options_daily p ON p.trade_date = c.trade_date
		WHERE c.contract_code = ?
		  AND p.contract_code = ?
		  AND c.trade_date BETWEEN ? AND ?
		  AND p.trade_date BETWEEN ? AND ?
		  AND c.close IS NOT NULL
		  AND p.close IS NOT NULL
		ORDER BY c.trade_date
	`, callContract, putContract, compactDay(start), compactDay(end), compactDay(start), compactDay(end))
	if err != nil {
		return StraddleBacktestResult{}, fmt.Errorf("query straddle backtest rows: %w", err)
	}
	defer rows.Close()

	result := StraddleBacktestResult{
		StartDate:       start.Format(dayLayout),
		EndDate:         end.Format(dayLayout),
		CallContract:    callContract,
		PutContract:     putContract,
		CallQuantity:    callQty,
		PutQuantity:     putQty,
		CalculationNote: "收益按每日收盘价 * 张数 计算，未乘合约乘数",
		Rows:            make([]StraddleBacktestRow, 0, 256),
	}

	initialCost := 0.0
	for rows.Next() {
		var tradeDate string
		var callClose float64
		var putClose float64
		if err := rows.Scan(&tradeDate, &callClose, &putClose); err != nil {
			return StraddleBacktestResult{}, fmt.Errorf("scan straddle backtest row: %w", err)
		}

		callValue := callClose * float64(callQty)
		putValue := putClose * float64(putQty)
		totalValue := callValue + putValue
		if len(result.Rows) == 0 {
			initialCost = totalValue
			result.InitialCost = initialCost
			result.ActualStartDate = displayCompactDay(tradeDate)
		}

		result.Rows = append(result.Rows, StraddleBacktestRow{
			Date:        displayCompactDay(tradeDate),
			CallClose:   callClose,
			PutClose:    putClose,
			CallValue:   callValue,
			PutValue:    putValue,
			TotalValue:  totalValue,
			TotalProfit: totalValue - initialCost,
		})
	}
	if err := rows.Err(); err != nil {
		return StraddleBacktestResult{}, fmt.Errorf("iterate straddle backtest rows: %w", err)
	}
	if len(result.Rows) == 0 {
		return StraddleBacktestResult{}, errors.New("no overlapping close data for selected contracts and date range")
	}

	result.TradingDays = len(result.Rows)
	result.ActualEndDate = result.Rows[len(result.Rows)-1].Date
	result.FinalValue = result.Rows[len(result.Rows)-1].TotalValue
	result.TotalProfit = result.Rows[len(result.Rows)-1].TotalProfit
	return result, nil
}

func (store *MarketStore) RunContinuousStraddleBacktest(start, end time.Time, holdDays, minDTE int, sellProfit float64, restDays int, maxATRPercent float64) (ContinuousStraddleResult, error) {
	spots, err := loadSpotSeries(store.spotCSV)
	if err != nil {
		return ContinuousStraddleResult{}, err
	}
	spotDates := sortedSpotDates(spots, start, end)
	if len(spotDates) == 0 {
		return ContinuousStraddleResult{}, errors.New("no CSI1000 spot rows in selected date range")
	}

	result := ContinuousStraddleResult{
		StartDate:       start.Format(dayLayout),
		EndDate:         end.Format(dayLayout),
		HoldDays:        holdDays,
		MinDTE:          minDTE,
		SellProfit:      sellProfit,
		RestDays:        restDays,
		MaxATRPercent:   maxATRPercent,
		CalculationNote: "中证1000 MO 期权，按指数 close 选上下 ATM，按期权 close 建平仓；仅当当日 ATR% 低于阈值时开仓，未乘合约乘数",
		Events:          make([]ContinuousStraddleEvent, 0, 128),
	}

	realizedProfit := 0.0
	restRemaining := 0
	tradeProfits := make([]float64, 0, 64)
	strategyReturns := make([]float64, 0, len(spotDates))
	benchmarkReturns := make([]float64, 0, len(spotDates))
	strategyNAV := 1.0
	peakNAV := 1.0
	maxDrawdown := 0.0
	var prevSpotClose float64
	var position *continuousPosition
	var excludedExpiry *time.Time

	for _, date := range spotDates {
		result.TradingDays++
		key := date.Format(dayLayout)
		spot := spots[key]
		spotClose := spot.Close
		atrPct := spot.ATRPct
		excludedExpiry = nil
		strategyDailyReturn := 0.0

		if position != nil {
			callClose, putClose, ok, err := store.optionPairClose(date, position.CallContract, position.PutContract)
			if err != nil {
				return ContinuousStraddleResult{}, err
			}
			if ok {
				value := callClose + putClose
				if position.LastValue > 0 {
					strategyDailyReturn = value/position.LastValue - 1
				}
				position.LastValue = value
				position.LastDTE = daysBetween(date, position.Expiry)
				profit := value - position.EntryValue
				profitPct := profit / position.EntryValue
				spotChangePct := spotMovePct(position.EntrySpot, spotClose)
				daysHeld := daysBetween(position.EntryDate, date)
				reason := continuousSellReason(spotChangePct, daysHeld, position.LastDTE, sellProfit, holdDays)
				if reason != "" {
					realizedProfit += profit
					tradeProfits = append(tradeProfits, profit)
					result.Exits++
					if profit > 0 {
						result.WinningExits++
					}
					result.Events = append(result.Events, ContinuousStraddleEvent{
						Date:             key,
						Action:           "sell",
						Reason:           reason,
						CallContract:     position.CallContract,
						PutContract:      position.PutContract,
						SpotClose:        spotClose,
						ATRPct:           atrPct,
						SpotChangePct:    spotChangePct,
						CallClose:        callClose,
						PutClose:         putClose,
						PositionValue:    value,
						TradeProfit:      profit,
						TradeProfitPct:   profitPct,
						CumulativeProfit: realizedProfit,
						DaysHeld:         daysHeld,
						DaysToExpiry:     position.LastDTE,
					})
					if reason == "dte" {
						expiry := position.Expiry
						excludedExpiry = &expiry
						restRemaining = 0
					} else {
						restRemaining = restDays
					}
					position = nil
				}
			}
		}

		if position != nil {
			continue
		}
		if restRemaining > 0 {
			result.Events = append(result.Events, ContinuousStraddleEvent{
				Date:              key,
				Action:            "rest",
				Reason:            "rest_days",
				SpotClose:         spotClose,
				ATRPct:            atrPct,
				CumulativeProfit:  realizedProfit,
				RestDaysRemaining: restRemaining,
			})
			restRemaining--
			continue
		}
		if !spot.HasATRPct {
			result.Events = append(result.Events, ContinuousStraddleEvent{
				Date:             key,
				Action:           "wait",
				Reason:           "atr_missing",
				SpotClose:        spotClose,
				CumulativeProfit: realizedProfit,
			})
			continue
		}
		if atrPct >= maxATRPercent {
			result.Events = append(result.Events, ContinuousStraddleEvent{
				Date:             key,
				Action:           "wait",
				Reason:           "atr_limit",
				SpotClose:        spotClose,
				ATRPct:           atrPct,
				CumulativeProfit: realizedProfit,
			})
			continue
		}

		quotes, err := store.optionQuotesForDate(date, "MO")
		if err != nil {
			return ContinuousStraddleResult{}, err
		}
		callQuote, putQuote, ok := chooseContinuousATM(quotes, spotClose, minDTE, excludedExpiry)
		if !ok {
			continue
		}
		entryValue := callQuote.Close + putQuote.Close
		position = &continuousPosition{
			CallContract: callQuote.ContractCode,
			PutContract:  putQuote.ContractCode,
			EntryDate:    date,
			EntrySpot:    spotClose,
			Expiry:       callQuote.Expiry,
			EntryValue:   entryValue,
			LastValue:    entryValue,
			LastDTE:      callQuote.DaysToExpiry,
		}
		result.Entries++
		result.Events = append(result.Events, ContinuousStraddleEvent{
			Date:             key,
			Action:           "buy",
			Reason:           "entry",
			CallContract:     callQuote.ContractCode,
			PutContract:      putQuote.ContractCode,
			SpotClose:        spotClose,
			ATRPct:           atrPct,
			CallClose:        callQuote.Close,
			PutClose:         putQuote.Close,
			PositionValue:    entryValue,
			CumulativeProfit: realizedProfit,
			DaysHeld:         0,
			DaysToExpiry:     callQuote.DaysToExpiry,
		})

		benchmarkDailyReturn := 0.0
		if prevSpotClose > 0 {
			benchmarkDailyReturn = spotClose/prevSpotClose - 1
			strategyReturns = append(strategyReturns, strategyDailyReturn)
			benchmarkReturns = append(benchmarkReturns, benchmarkDailyReturn)
			strategyNAV *= 1 + strategyDailyReturn
			if strategyNAV > peakNAV {
				peakNAV = strategyNAV
			}
			drawdown := 0.0
			if peakNAV > 0 {
				drawdown = 1 - strategyNAV/peakNAV
			}
			if drawdown > maxDrawdown {
				maxDrawdown = drawdown
			}
		}
		prevSpotClose = spotClose
	}

	result.RealizedProfit = realizedProfit
	if position != nil {
		result.FinalPositionOpen = true
		result.FinalValue = position.LastValue
		result.UnrealizedProfit = position.LastValue - position.EntryValue
	}
	result.ProfitLossRatio = profitLossRatio(tradeProfits)
	result.SharpeRatio = sharpeRatio(strategyReturns)
	result.Alpha = annualizedAlpha(strategyReturns, benchmarkReturns)
	result.MaxDrawdown = maxDrawdown
	result.TotalProfit = result.RealizedProfit + result.UnrealizedProfit
	if result.Entries == 0 {
		return ContinuousStraddleResult{}, errors.New("no strategy entry; check date range, min_dte, and max_atr_pct")
	}
	return result, nil
}

func profitLossRatio(tradeProfits []float64) float64 {
	winSum := 0.0
	winCount := 0
	lossSum := 0.0
	lossCount := 0
	for _, profit := range tradeProfits {
		if profit > 0 {
			winSum += profit
			winCount++
			continue
		}
		if profit < 0 {
			lossSum += math.Abs(profit)
			lossCount++
		}
	}
	if winCount == 0 || lossCount == 0 {
		return 0
	}
	return (winSum / float64(winCount)) / (lossSum / float64(lossCount))
}

func sharpeRatio(returns []float64) float64 {
	if len(returns) < 2 {
		return 0
	}
	mean := average(returns)
	std := stddevSample(returns, mean)
	if std == 0 {
		return 0
	}
	return mean / std * math.Sqrt(252)
}

func annualizedAlpha(strategyReturns []float64, benchmarkReturns []float64) float64 {
	count := min(len(strategyReturns), len(benchmarkReturns))
	if count < 2 {
		return 0
	}
	strategy := strategyReturns[:count]
	benchmark := benchmarkReturns[:count]
	meanStrategy := average(strategy)
	meanBenchmark := average(benchmark)
	varianceBenchmark := varianceSample(benchmark, meanBenchmark)
	if varianceBenchmark == 0 {
		return (meanStrategy - meanBenchmark) * 252
	}
	beta := covarianceSample(strategy, benchmark, meanStrategy, meanBenchmark) / varianceBenchmark
	alphaDaily := meanStrategy - beta*meanBenchmark
	return alphaDaily * 252
}

func average(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	sum := 0.0
	for _, value := range values {
		sum += value
	}
	return sum / float64(len(values))
}

func stddevSample(values []float64, mean float64) float64 {
	variance := varianceSample(values, mean)
	if variance <= 0 {
		return 0
	}
	return math.Sqrt(variance)
}

func varianceSample(values []float64, mean float64) float64 {
	if len(values) < 2 {
		return 0
	}
	sum := 0.0
	for _, value := range values {
		delta := value - mean
		sum += delta * delta
	}
	return sum / float64(len(values)-1)
}

func covarianceSample(left []float64, right []float64, meanLeft float64, meanRight float64) float64 {
	count := min(len(left), len(right))
	if count < 2 {
		return 0
	}
	sum := 0.0
	for i := 0; i < count; i++ {
		sum += (left[i] - meanLeft) * (right[i] - meanRight)
	}
	return sum / float64(count-1)
}

func continuousSellReason(spotChangePct float64, daysHeld int, daysToExpiry int, sellProfit float64, holdDays int) string {
	if spotChangePct > sellProfit {
		return "spot_move"
	}
	if daysHeld >= holdDays {
		return "hold_days"
	}
	if daysToExpiry < 20 {
		return "dte"
	}
	return ""
}

func spotMovePct(entrySpot float64, currentSpot float64) float64 {
	if entrySpot <= 0 || currentSpot <= 0 {
		return 0
	}
	return math.Abs(currentSpot/entrySpot - 1)
}

func sortedSpotDates(spots map[string]spotSnapshot, start, end time.Time) []time.Time {
	dates := make([]time.Time, 0, len(spots))
	for key := range spots {
		date, err := time.ParseInLocation(dayLayout, key, time.Local)
		if err != nil {
			continue
		}
		if date.Before(start) || date.After(end) {
			continue
		}
		dates = append(dates, date)
	}
	sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })
	return dates
}

func (store *MarketStore) optionPairClose(date time.Time, callContract, putContract string) (float64, float64, bool, error) {
	db, err := store.optionsDatabase()
	if err != nil {
		return 0, 0, false, err
	}
	row := db.QueryRow(`
		SELECT c.close, p.close
		FROM options_daily c
		JOIN options_daily p ON p.trade_date = c.trade_date
		WHERE c.trade_date = ?
		  AND c.contract_code = ?
		  AND p.contract_code = ?
		  AND c.close IS NOT NULL
		  AND p.close IS NOT NULL
	`, compactDay(date), callContract, putContract)
	var callClose float64
	var putClose float64
	if err := row.Scan(&callClose, &putClose); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, false, nil
		}
		return 0, 0, false, fmt.Errorf("query option pair close: %w", err)
	}
	return callClose, putClose, true, nil
}

func (store *MarketStore) optionQuotesForDate(date time.Time, product string) ([]optionQuote, error) {
	db, err := store.optionsDatabase()
	if err != nil {
		return nil, err
	}
	rows, err := db.Query(`
		SELECT contract_code, close
		FROM options_daily
		WHERE trade_date = ?
		  AND contract_code LIKE ?
		  AND close IS NOT NULL
		ORDER BY contract_code
	`, compactDay(date), product+"____-%")
	if err != nil {
		return nil, fmt.Errorf("query option quotes: %w", err)
	}
	defer rows.Close()

	quotes := make([]optionQuote, 0, 256)
	for rows.Next() {
		var code string
		var closePrice float64
		if err := rows.Scan(&code, &closePrice); err != nil {
			return nil, fmt.Errorf("scan option quote: %w", err)
		}
		choice, ok := optionContractChoiceFromCode(code)
		if !ok {
			continue
		}
		strike, err := strconv.ParseFloat(choice.Strike, 64)
		if err != nil {
			continue
		}
		expiry, err := expiryFromOptionChoice(choice)
		if err != nil {
			continue
		}
		quotes = append(quotes, optionQuote{
			ContractCode: strings.ToUpper(code),
			OptionType:   choice.OptionType,
			Strike:       strike,
			Close:        closePrice,
			Expiry:       expiry,
			DaysToExpiry: daysBetween(date, expiry),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate option quotes: %w", err)
	}
	return quotes, nil
}

func chooseContinuousATM(quotes []optionQuote, spotClose float64, minDTE int, excludedExpiry *time.Time) (optionQuote, optionQuote, bool) {
	expiries := make(map[string]time.Time)
	for _, quote := range quotes {
		if quote.DaysToExpiry <= minDTE {
			continue
		}
		if excludedExpiry != nil && quote.Expiry.Equal(*excludedExpiry) {
			continue
		}
		expiries[quote.Expiry.Format(dayLayout)] = quote.Expiry
	}
	ordered := make([]time.Time, 0, len(expiries))
	for _, expiry := range expiries {
		ordered = append(ordered, expiry)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Before(ordered[j]) })

	for _, expiry := range ordered {
		calls := make([]optionQuote, 0, 32)
		puts := make([]optionQuote, 0, 32)
		for _, quote := range quotes {
			if !quote.Expiry.Equal(expiry) || quote.DaysToExpiry <= minDTE {
				continue
			}
			if quote.OptionType == "C" {
				calls = append(calls, quote)
			}
			if quote.OptionType == "P" {
				puts = append(puts, quote)
			}
		}
		callQuote, okCall := chooseCallAboveSpot(calls, spotClose)
		putQuote, okPut := choosePutBelowSpot(puts, spotClose)
		if okCall && okPut {
			return callQuote, putQuote, true
		}
	}
	return optionQuote{}, optionQuote{}, false
}

func chooseCallAboveSpot(quotes []optionQuote, spotClose float64) (optionQuote, bool) {
	var selected optionQuote
	ok := false
	for _, quote := range quotes {
		if !ok {
			selected = quote
			ok = true
			continue
		}
		selectedAbove := selected.Strike >= spotClose
		quoteAbove := quote.Strike >= spotClose
		if quoteAbove && (!selectedAbove || quote.Strike < selected.Strike) {
			selected = quote
			continue
		}
		if !quoteAbove && !selectedAbove && math.Abs(quote.Strike-spotClose) < math.Abs(selected.Strike-spotClose) {
			selected = quote
		}
	}
	return selected, ok
}

func choosePutBelowSpot(quotes []optionQuote, spotClose float64) (optionQuote, bool) {
	var selected optionQuote
	ok := false
	for _, quote := range quotes {
		if !ok {
			selected = quote
			ok = true
			continue
		}
		selectedBelow := selected.Strike <= spotClose
		quoteBelow := quote.Strike <= spotClose
		if quoteBelow && (!selectedBelow || quote.Strike > selected.Strike) {
			selected = quote
			continue
		}
		if !quoteBelow && !selectedBelow && math.Abs(quote.Strike-spotClose) < math.Abs(selected.Strike-spotClose) {
			selected = quote
		}
	}
	return selected, ok
}

func expiryFromOptionChoice(choice OptionContractChoice) (time.Time, error) {
	if len(choice.Month) != 4 {
		return time.Time{}, fmt.Errorf("invalid option month: %s", choice.ContractCode)
	}
	yy, err := strconv.Atoi(choice.Month[:2])
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid option year: %s", choice.ContractCode)
	}
	mm, err := strconv.Atoi(choice.Month[2:])
	if err != nil || mm < 1 || mm > 12 {
		return time.Time{}, fmt.Errorf("invalid option month: %s", choice.ContractCode)
	}
	return thirdFriday(2000+yy, time.Month(mm)), nil
}

func (store *MarketStore) buildContractIndexLocked() error {
	files, err := dataFiles(store.dataDir)
	if err != nil {
		return err
	}
	contracts := make(map[string]ContractInfo)
	for _, file := range files {
		if err := readMarketFile(file, func(record DailyRecord) {
			product, kind, optionType, strike, ok := parseContractCode(record.Code)
			if !ok {
				return
			}
			info := contracts[record.Code]
			if info.Code == "" {
				info = ContractInfo{
					Code:       record.Code,
					Product:    product,
					Kind:       kind,
					OptionType: optionType,
					Strike:     strike,
					FirstDate:  record.Date,
					LastDate:   record.Date,
				}
			}
			if record.Date < info.FirstDate {
				info.FirstDate = record.Date
			}
			if record.Date > info.LastDate {
				info.LastDate = record.Date
			}
			info.Rows++
			contracts[record.Code] = info
		}); err != nil {
			return err
		}
	}
	store.contracts = contracts
	store.contractsReady = true
	return nil
}
