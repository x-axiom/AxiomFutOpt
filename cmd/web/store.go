package main

import (
	"database/sql"
	"errors"
	"fmt"
	"sort"
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
