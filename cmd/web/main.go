package main

import (
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
)

const dayLayout = "2006-01-02"

var (
	contractCodePattern = regexp.MustCompile(`^([A-Z]{1,3})(\d{4})(?:-([CP])-(\d+(?:\.\d+)?))?$`)
	imCodePattern       = regexp.MustCompile(`^IM(\d{2})(\d{2})$`)
)

type App struct {
	store      *MarketStore
	staticRoot http.Handler
}

type DataFile struct {
	Path string
	Date time.Time
}

type ContractInfo struct {
	Code       string `json:"code"`
	Product    string `json:"product"`
	Kind       string `json:"kind"`
	OptionType string `json:"option_type,omitempty"`
	Strike     string `json:"strike,omitempty"`
	FirstDate  string `json:"first_date"`
	LastDate   string `json:"last_date"`
	Rows       int    `json:"rows"`
}

type DailyRecord struct {
	Date         string `json:"date"`
	Code         string `json:"code"`
	Open         string `json:"open"`
	High         string `json:"high"`
	Low          string `json:"low"`
	Volume       string `json:"volume"`
	Amount       string `json:"amount"`
	OpenInterest string `json:"open_interest"`
	OIChange     string `json:"oi_change"`
	Close        string `json:"close"`
	Settle       string `json:"settle"`
	PrevSettle   string `json:"prev_settle"`
	Change1      string `json:"change1"`
	Change2      string `json:"change2"`
	Delta        string `json:"delta"`
}

type BacktestData struct {
	SpotClose map[string]float64
	Futures   map[string][]IMBar
}

type IMBar struct {
	Date         time.Time
	Code         string
	Settle       float64
	Expiry       time.Time
	Volume       int64
	OpenInterest int64
}

type Candidate struct {
	Bar             IMBar
	AnnualizedBasis float64
	DaysToExpiry    int
	MeetsThreshold  bool
}

type BacktestEvent struct {
	Date             string  `json:"date"`
	Action           string  `json:"action"`
	Code             string  `json:"code"`
	Price            float64 `json:"price"`
	SpotClose        float64 `json:"spot_close"`
	AnnualizedBasis  float64 `json:"annualized_basis"`
	DaysToExpiry     int     `json:"days_to_expiry"`
	CumulativeProfit float64 `json:"cumulative_profit"`
}

type BacktestResult struct {
	BasisYieldThreshold float64         `json:"basis_yield_threshold"`
	RollDays            int             `json:"roll_days"`
	Multiplier          float64         `json:"multiplier"`
	StartDate           string          `json:"start_date"`
	EndDate             string          `json:"end_date"`
	HoldingDays         int             `json:"holding_days"`
	Entries             int             `json:"entries"`
	Rolls               int             `json:"rolls"`
	FinalContract       string          `json:"final_contract"`
	FinalSettle         float64         `json:"final_settle"`
	TotalProfit         float64         `json:"total_profit"`
	Events              []BacktestEvent `json:"events"`
}

type OptionContractChoice struct {
	ContractCode string `json:"contract_code"`
	Product      string `json:"product"`
	Month        string `json:"month"`
	OptionType   string `json:"option_type"`
	Strike       string `json:"strike"`
	FirstDate    string `json:"first_date"`
	LastDate     string `json:"last_date"`
}

type StraddleBacktestRow struct {
	Date        string  `json:"date"`
	CallClose   float64 `json:"call_close"`
	PutClose    float64 `json:"put_close"`
	CallValue   float64 `json:"call_value"`
	PutValue    float64 `json:"put_value"`
	TotalValue  float64 `json:"total_value"`
	TotalProfit float64 `json:"total_profit"`
}

type StraddleBacktestResult struct {
	StartDate       string                `json:"start_date"`
	EndDate         string                `json:"end_date"`
	ActualStartDate string                `json:"actual_start_date"`
	ActualEndDate   string                `json:"actual_end_date"`
	CallContract    string                `json:"call_contract"`
	PutContract     string                `json:"put_contract"`
	CallQuantity    int                   `json:"call_quantity"`
	PutQuantity     int                   `json:"put_quantity"`
	TradingDays     int                   `json:"trading_days"`
	InitialCost     float64               `json:"initial_cost"`
	FinalValue      float64               `json:"final_value"`
	TotalProfit     float64               `json:"total_profit"`
	CalculationNote string                `json:"calculation_note"`
	Rows            []StraddleBacktestRow `json:"rows"`
}

type ContinuousStraddleEvent struct {
	Date              string  `json:"date"`
	Action            string  `json:"action"`
	Reason            string  `json:"reason,omitempty"`
	CallContract      string  `json:"call_contract,omitempty"`
	PutContract       string  `json:"put_contract,omitempty"`
	SpotClose         float64 `json:"spot_close"`
	ATRPct            float64 `json:"atr_pct"`
	SpotChangePct     float64 `json:"spot_change_pct"`
	CallClose         float64 `json:"call_close"`
	PutClose          float64 `json:"put_close"`
	PositionValue     float64 `json:"position_value"`
	TradeProfit       float64 `json:"trade_profit"`
	TradeProfitPct    float64 `json:"trade_profit_pct"`
	CumulativeProfit  float64 `json:"cumulative_profit"`
	DaysHeld          int     `json:"days_held"`
	DaysToExpiry      int     `json:"days_to_expiry"`
	RestDaysRemaining int     `json:"rest_days_remaining,omitempty"`
}

type ContinuousStraddleResult struct {
	StartDate         string                    `json:"start_date"`
	EndDate           string                    `json:"end_date"`
	HoldDays          int                       `json:"hold_days"`
	MinDTE            int                       `json:"min_dte"`
	SellProfit        float64                   `json:"sell_profit"`
	RestDays          int                       `json:"rest_days"`
	ATRFilterMode     string                    `json:"atr_filter_mode"`
	MaxATRPercent     float64                   `json:"max_atr_pct"`
	BackoffDate       string                    `json:"backoff_date"`
	TradingDays       int                       `json:"trading_days"`
	Entries           int                       `json:"entries"`
	Exits             int                       `json:"exits"`
	WinningExits      int                       `json:"winning_exits"`
	TotalProfit       float64                   `json:"total_profit"`
	RealizedProfit    float64                   `json:"realized_profit"`
	UnrealizedProfit  float64                   `json:"unrealized_profit"`
	ProfitLossRatio   float64                   `json:"profit_loss_ratio"`
	SharpeRatio       float64                   `json:"sharpe_ratio"`
	Alpha             float64                   `json:"alpha"`
	MaxDrawdown       float64                   `json:"max_drawdown"`
	FinalPositionOpen bool                      `json:"final_position_open"`
	FinalValue        float64                   `json:"final_value"`
	CalculationNote   string                    `json:"calculation_note"`
	Events            []ContinuousStraddleEvent `json:"events"`
}

type spotSnapshot struct {
	Close     float64
	ATRPct    float64
	HasATRPct bool
}

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dataDir := flag.String("data-dir", "extracted", "directory containing CFFEX daily CSV files")
	spotCSV := flag.String("spot-csv", "data/csi1000_daily.csv", "CSI 1000 daily OHLC CSV")
	optionsDB := flag.String("options-db", "data/duckdb/market.duckdb", "DuckDB file containing options_daily")
	webDir := flag.String("web-dir", "cmd/web/data", "directory containing frontend static assets")
	flag.Parse()

	store := &MarketStore{
		dataDir:       *dataDir,
		spotCSV:       *spotCSV,
		optionsDBPath: *optionsDB,
		contracts:     make(map[string]ContractInfo),
		historyCache:  make(map[string][]DailyRecord),
	}
	app := &App{store: store, staticRoot: http.FileServer(http.Dir(*webDir))}

	mux := http.NewServeMux()
	mux.HandleFunc("/", app.handleIndex)
	mux.HandleFunc("/api/contracts", app.handleContracts)
	mux.HandleFunc("/api/history", app.handleHistory)
	mux.HandleFunc("/api/backtest", app.handleBacktest)
	mux.HandleFunc("/api/straddle/contracts", app.handleStraddleContracts)
	mux.HandleFunc("/api/straddle/backtest", app.handleStraddleBacktest)
	mux.HandleFunc("/api/continuous-straddle/backtest", app.handleContinuousStraddleBacktest)

	log.Printf("serving on http://localhost%s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func (app *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	app.staticRoot.ServeHTTP(w, r)
}

func (app *App) handleContracts(w http.ResponseWriter, r *http.Request) {
	contracts, err := app.store.Contracts()
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}

	q := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("q")))
	product := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("product")))
	kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("kind")))
	limit := intParam(r, "limit", 200)

	filtered := make([]ContractInfo, 0, min(limit, len(contracts)))
	for _, contract := range contracts {
		if q != "" && !strings.Contains(contract.Code, q) {
			continue
		}
		if product != "" && contract.Product != product {
			continue
		}
		if kind != "" && contract.Kind != kind {
			continue
		}
		filtered = append(filtered, contract)
		if limit > 0 && len(filtered) >= limit {
			break
		}
	}
	writeJSON(w, map[string]any{"contracts": filtered, "count": len(filtered)})
}

func (app *App) handleHistory(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(strings.TrimSpace(r.URL.Query().Get("code")))
	if code == "" {
		writeError(w, errors.New("missing code"), http.StatusBadRequest)
		return
	}
	limit := intParam(r, "limit", 500)

	history, err := app.store.History(code)
	if err != nil {
		writeError(w, err, http.StatusNotFound)
		return
	}
	contract := infoFromHistory(history)
	if limit > 0 && len(history) > limit {
		history = history[len(history)-limit:]
	}
	writeJSON(w, map[string]any{"contract": contract, "records": history, "count": len(history)})
}

func (app *App) handleBacktest(w http.ResponseWriter, r *http.Request) {
	basisYield := floatParam(r, "basis_yield", 0.06)
	rollDays := intParam(r, "roll_days", 5)
	multiplier := floatParam(r, "multiplier", 200)
	if rollDays < 0 {
		writeError(w, errors.New("roll_days must be >= 0"), http.StatusBadRequest)
		return
	}
	if multiplier <= 0 {
		writeError(w, errors.New("multiplier must be > 0"), http.StatusBadRequest)
		return
	}

	start, err := optionalDate(r.URL.Query().Get("start"))
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	end, err := optionalDate(r.URL.Query().Get("end"))
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}

	data, err := app.store.BacktestData()
	if err != nil {
		writeError(w, err, http.StatusInternalServerError)
		return
	}
	result, err := runBacktest(data.Futures, data.SpotClose, basisYield, rollDays, multiplier, start, end)
	if err != nil {
		writeError(w, err, http.StatusBadRequest)
		return
	}
	writeJSON(w, result)
}

func dataFiles(root string) ([]DataFile, error) {
	files := make([]DataFile, 0, 4096)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".csv") {
			return nil
		}
		date, ok := dateFromFileName(entry.Name())
		if !ok {
			return nil
		}
		files = append(files, DataFile{Path: path, Date: date})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].Date.Equal(files[j].Date) {
			return files[i].Path < files[j].Path
		}
		return files[i].Date.Before(files[j].Date)
	})
	return files, nil
}

func readMarketFile(file DataFile, visit func(DailyRecord)) error {
	f, err := os.Open(file.Path)
	if err != nil {
		return fmt.Errorf("open %s: %w", file.Path, err)
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1
	if _, err := reader.Read(); err != nil {
		return fmt.Errorf("read header %s: %w", file.Path, err)
	}
	for {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("read row %s: %w", file.Path, err)
		}
		record, ok := recordFromRow(row, file.Date)
		if ok {
			visit(record)
		}
	}
	return nil
}

func recordFromRow(row []string, date time.Time) (DailyRecord, bool) {
	if len(row) < 14 {
		return DailyRecord{}, false
	}
	code := strings.ToUpper(strings.TrimSpace(strings.TrimPrefix(row[0], "\ufeff")))
	if !contractCodePattern.MatchString(code) {
		return DailyRecord{}, false
	}
	return DailyRecord{
		Date:         date.Format(dayLayout),
		Code:         code,
		Open:         field(row, 1),
		High:         field(row, 2),
		Low:          field(row, 3),
		Volume:       field(row, 4),
		Amount:       field(row, 5),
		OpenInterest: field(row, 6),
		OIChange:     field(row, 7),
		Close:        field(row, 8),
		Settle:       field(row, 9),
		PrevSettle:   field(row, 10),
		Change1:      field(row, 11),
		Change2:      field(row, 12),
		Delta:        field(row, 13),
	}, true
}

func field(row []string, index int) string {
	if index >= len(row) {
		return ""
	}
	return strings.TrimSpace(row[index])
}

func parseContractCode(code string) (product string, kind string, optionType string, strike string, ok bool) {
	matches := contractCodePattern.FindStringSubmatch(code)
	if matches == nil {
		return "", "", "", "", false
	}
	product = matches[1]
	if matches[3] == "" {
		return product, "future", "", "", true
	}
	return product, "option", matches[3], matches[4], true
}

func optionContractChoiceFromCode(code string) (OptionContractChoice, bool) {
	matches := contractCodePattern.FindStringSubmatch(strings.ToUpper(strings.TrimSpace(code)))
	if matches == nil || matches[3] == "" {
		return OptionContractChoice{}, false
	}
	return OptionContractChoice{
		ContractCode: strings.ToUpper(strings.TrimSpace(code)),
		Product:      matches[1],
		Month:        matches[2],
		OptionType:   matches[3],
		Strike:       matches[4],
	}, true
}

func infoFromHistory(records []DailyRecord) ContractInfo {
	if len(records) == 0 {
		return ContractInfo{}
	}
	product, kind, optionType, strike, _ := parseContractCode(records[0].Code)
	return ContractInfo{
		Code:       records[0].Code,
		Product:    product,
		Kind:       kind,
		OptionType: optionType,
		Strike:     strike,
		FirstDate:  records[0].Date,
		LastDate:   records[len(records)-1].Date,
		Rows:       len(records),
	}
}

func loadSpotClose(path string) (map[string]float64, error) {
	series, err := loadSpotSeries(path)
	if err != nil {
		return nil, err
	}
	spots := make(map[string]float64, len(series))
	for date, snapshot := range series {
		spots[date] = snapshot.Close
	}
	return spots, nil
}

func loadSpotSeries(path string) (map[string]spotSnapshot, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open spot csv: %w", err)
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1
	header, err := reader.Read()
	if err != nil {
		return nil, fmt.Errorf("read spot header: %w", err)
	}
	atrPctIndex := -1
	for idx, name := range header {
		if strings.EqualFold(strings.TrimSpace(name), "atr_pct") {
			atrPctIndex = idx
			break
		}
	}
	spots := make(map[string]spotSnapshot)
	for {
		row, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read spot row: %w", err)
		}
		if len(row) < 5 {
			continue
		}
		date := strings.TrimSpace(row[0])
		closePrice, err := parseFloat(row[4])
		if err != nil || closePrice <= 0 {
			continue
		}
		snapshot := spotSnapshot{Close: closePrice}
		if atrPctIndex >= 0 && atrPctIndex < len(row) {
			if atrPct, err := parseFloat(row[atrPctIndex]); err == nil && atrPct > 0 {
				snapshot.ATRPct = atrPct
				snapshot.HasATRPct = true
			}
		}
		spots[date] = snapshot
	}
	if len(spots) == 0 {
		return nil, errors.New("spot csv has no valid rows")
	}
	return spots, nil
}

func loadIMFutures(root string) (map[string][]IMBar, []time.Time, error) {
	files, err := dataFiles(root)
	if err != nil {
		return nil, nil, err
	}
	byDate := make(map[string][]IMBar)
	dateSet := make(map[string]time.Time)
	for _, file := range files {
		if err := readMarketFile(file, func(record DailyRecord) {
			if !imCodePattern.MatchString(record.Code) {
				return
			}
			settle, err := parseFloat(record.Settle)
			if err != nil || settle <= 0 {
				return
			}
			expiry, err := expiryFromIMCode(record.Code)
			if err != nil {
				return
			}
			key := file.Date.Format(dayLayout)
			byDate[key] = append(byDate[key], IMBar{
				Date:         file.Date,
				Code:         record.Code,
				Settle:       settle,
				Expiry:       expiry,
				Volume:       parseInt(record.Volume),
				OpenInterest: parseInt(record.OpenInterest),
			})
			dateSet[key] = file.Date
		}); err != nil {
			return nil, nil, err
		}
	}
	if len(byDate) == 0 {
		return nil, nil, errors.New("no IM futures rows found")
	}
	tradingDays := make([]time.Time, 0, len(dateSet))
	for _, date := range dateSet {
		tradingDays = append(tradingDays, date)
	}
	sort.Slice(tradingDays, func(i, j int) bool { return tradingDays[i].Before(tradingDays[j]) })
	return byDate, tradingDays, nil
}

func runBacktest(futures map[string][]IMBar, spots map[string]float64, threshold float64, rollDays int, multiplier float64, start, end time.Time) (BacktestResult, error) {
	dates := sortedDates(futures)
	result := BacktestResult{BasisYieldThreshold: threshold, RollDays: rollDays, Multiplier: multiplier}
	currentCode := ""
	previousSettle := 0.0

	for _, date := range dates {
		if !start.IsZero() && date.Before(start) {
			continue
		}
		if !end.IsZero() && date.After(end) {
			continue
		}
		key := date.Format(dayLayout)
		spotClose, ok := spots[key]
		if !ok {
			continue
		}
		bars := futures[key]
		byCode := make(map[string]IMBar, len(bars))
		for _, bar := range bars {
			byCode[bar.Code] = bar
		}

		needRoll := currentCode == ""
		if currentCode != "" {
			bar, ok := byCode[currentCode]
			if ok {
				if previousSettle > 0 {
					result.TotalProfit += (bar.Settle - previousSettle) * multiplier
				}
				previousSettle = bar.Settle
				if daysBetween(date, bar.Expiry) <= rollDays {
					needRoll = true
				}
			} else {
				needRoll = true
			}
		}

		if needRoll {
			candidate, ok := chooseContract(bars, spotClose, date, threshold, rollDays)
			if !ok {
				continue
			}
			action := "entry"
			if currentCode == "" {
				result.Entries++
			} else if candidate.Bar.Code != currentCode {
				action = "roll"
				result.Rolls++
			}
			currentCode = candidate.Bar.Code
			previousSettle = candidate.Bar.Settle
			if result.StartDate == "" {
				result.StartDate = key
			}
			result.Events = append(result.Events, BacktestEvent{
				Date:             key,
				Action:           action,
				Code:             candidate.Bar.Code,
				Price:            candidate.Bar.Settle,
				SpotClose:        spotClose,
				AnnualizedBasis:  candidate.AnnualizedBasis,
				DaysToExpiry:     candidate.DaysToExpiry,
				CumulativeProfit: result.TotalProfit,
			})
		}

		if currentCode != "" {
			result.HoldingDays++
			result.EndDate = key
			result.FinalContract = currentCode
			result.FinalSettle = previousSettle
		}
	}
	if currentCode == "" {
		return BacktestResult{}, errors.New("no position opened; check data and date range")
	}
	return result, nil
}

func chooseContract(bars []IMBar, spotClose float64, date time.Time, threshold float64, rollDays int) (Candidate, bool) {
	candidates := make([]Candidate, 0, len(bars))
	for _, bar := range bars {
		days := daysBetween(date, bar.Expiry)
		if days <= rollDays || bar.Settle <= 0 || spotClose <= 0 {
			continue
		}
		yield := annualizedDiscountYield(spotClose, bar.Settle, days)
		candidates = append(candidates, Candidate{Bar: bar, AnnualizedBasis: yield, DaysToExpiry: days, MeetsThreshold: yield >= threshold})
	}
	if len(candidates) == 0 {
		return Candidate{}, false
	}
	sort.Slice(candidates, func(i, j int) bool {
		left := candidates[i]
		right := candidates[j]
		if left.MeetsThreshold != right.MeetsThreshold {
			return left.MeetsThreshold
		}
		if math.Abs(left.AnnualizedBasis-right.AnnualizedBasis) > 1e-12 {
			return left.AnnualizedBasis > right.AnnualizedBasis
		}
		if !left.Bar.Expiry.Equal(right.Bar.Expiry) {
			return left.Bar.Expiry.Before(right.Bar.Expiry)
		}
		if left.Bar.OpenInterest != right.Bar.OpenInterest {
			return left.Bar.OpenInterest > right.Bar.OpenInterest
		}
		return left.Bar.Volume > right.Bar.Volume
	})
	return candidates[0], true
}

func annualizedDiscountYield(spotClose, futuresSettle float64, daysToExpiry int) float64 {
	return (spotClose/futuresSettle - 1) * 365 / float64(daysToExpiry)
}

func adjustExpiries(futures map[string][]IMBar, tradingDays []time.Time) {
	cache := make(map[string]time.Time)
	for dateKey, bars := range futures {
		for i := range bars {
			key := bars[i].Expiry.Format(dayLayout)
			adjusted, ok := cache[key]
			if !ok {
				adjusted = nextTradingDayOnOrAfter(bars[i].Expiry, tradingDays)
				cache[key] = adjusted
			}
			bars[i].Expiry = adjusted
		}
		futures[dateKey] = bars
	}
}

func nextTradingDayOnOrAfter(date time.Time, tradingDays []time.Time) time.Time {
	idx := sort.Search(len(tradingDays), func(i int) bool { return !tradingDays[i].Before(date) })
	if idx == len(tradingDays) {
		return date
	}
	return tradingDays[idx]
}

func expiryFromIMCode(code string) (time.Time, error) {
	matches := imCodePattern.FindStringSubmatch(code)
	if matches == nil {
		return time.Time{}, fmt.Errorf("invalid IM code: %s", code)
	}
	yy, _ := strconv.Atoi(matches[1])
	mm, _ := strconv.Atoi(matches[2])
	if mm < 1 || mm > 12 {
		return time.Time{}, fmt.Errorf("invalid IM contract month: %s", code)
	}
	return thirdFriday(2000+yy, time.Month(mm)), nil
}

func thirdFriday(year int, month time.Month) time.Time {
	date := time.Date(year, month, 1, 0, 0, 0, 0, time.Local)
	for date.Weekday() != time.Friday {
		date = date.AddDate(0, 0, 1)
	}
	return date.AddDate(0, 0, 14)
}

func sortedDates[T any](byDate map[string][]T) []time.Time {
	dates := make([]time.Time, 0, len(byDate))
	for key := range byDate {
		date, err := time.ParseInLocation(dayLayout, key, time.Local)
		if err == nil {
			dates = append(dates, date)
		}
	}
	sort.Slice(dates, func(i, j int) bool { return dates[i].Before(dates[j]) })
	return dates
}

func dateFromFileName(name string) (time.Time, bool) {
	if len(name) < 8 {
		return time.Time{}, false
	}
	date, err := time.ParseInLocation("20060102", name[:8], time.Local)
	return date, err == nil
}

func optionalDate(raw string) (time.Time, error) {
	if strings.TrimSpace(raw) == "" {
		return time.Time{}, nil
	}
	date, err := time.ParseInLocation(dayLayout, raw, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid date %q, want YYYY-MM-DD", raw)
	}
	return date, nil
}

func requiredDate(raw string, fieldName string) (time.Time, error) {
	date, err := optionalDate(raw)
	if err != nil {
		return time.Time{}, err
	}
	if date.IsZero() {
		return time.Time{}, fmt.Errorf("missing %s", fieldName)
	}
	return date, nil
}

func compactDay(date time.Time) string {
	return date.Format("20060102")
}

func displayCompactDay(raw string) string {
	if len(raw) != 8 {
		return raw
	}
	date, err := time.ParseInLocation("20060102", raw, time.Local)
	if err != nil {
		return raw
	}
	return date.Format(dayLayout)
}

func (store *MarketStore) optionsDatabase() (*sql.DB, error) {
	store.mu.Lock()
	defer store.mu.Unlock()

	if store.optionsDB != nil {
		return store.optionsDB, nil
	}
	db, err := sql.Open("duckdb", store.optionsDBPath)
	if err != nil {
		return nil, fmt.Errorf("open options duckdb: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("ping options duckdb: %w", err)
	}
	store.optionsDB = db
	return store.optionsDB, nil
}

func daysBetween(start, end time.Time) int {
	start = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.Local)
	end = time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.Local)
	return int(end.Sub(start).Hours() / 24)
}

func parseFloat(raw string) (float64, error) {
	value := strings.TrimSpace(raw)
	if value == "" || value == "--" {
		return 0, errors.New("empty number")
	}
	return strconv.ParseFloat(value, 64)
}

func parseInt(raw string) int64 {
	value, err := parseFloat(raw)
	if err != nil {
		return 0
	}
	return int64(math.Round(value))
}

func intParam(r *http.Request, name string, fallback int) int {
	value := strings.TrimSpace(r.URL.Query().Get(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}

func floatParam(r *http.Request, name string, fallback float64) float64 {
	value := strings.TrimSpace(r.URL.Query().Get(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(value); err != nil {
		log.Printf("write json: %v", err)
	}
}

func writeError(w http.ResponseWriter, err error, status int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}
