package main

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html/template"
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
	"sync"
	"time"
)

const dayLayout = "2006-01-02"

var (
	contractCodePattern = regexp.MustCompile(`^([A-Z]{1,3})(\d{4})(?:-([CP])-(\d+(?:\.\d+)?))?$`)
	imCodePattern       = regexp.MustCompile(`^IM(\d{2})(\d{2})$`)
	indexTemplate       = template.Must(template.New("index").Parse(indexHTML))
)

type App struct {
	store *MarketStore
}

type MarketStore struct {
	dataDir string
	spotCSV string

	mu             sync.Mutex
	contractsReady bool
	contracts      map[string]ContractInfo
	historyCache   map[string][]DailyRecord
	backtestData   *BacktestData
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

func main() {
	addr := flag.String("addr", ":8080", "HTTP listen address")
	dataDir := flag.String("data-dir", "extracted", "directory containing CFFEX daily CSV files")
	spotCSV := flag.String("spot-csv", "data/csi1000_000852_daily_ohlc_since_launch.csv", "CSI 1000 daily OHLC CSV")
	flag.Parse()

	store := &MarketStore{
		dataDir:      *dataDir,
		spotCSV:      *spotCSV,
		contracts:    make(map[string]ContractInfo),
		historyCache: make(map[string][]DailyRecord),
	}
	app := &App{store: store}

	mux := http.NewServeMux()
	mux.HandleFunc("/", app.handleIndex)
	mux.HandleFunc("/api/contracts", app.handleContracts)
	mux.HandleFunc("/api/history", app.handleHistory)
	mux.HandleFunc("/api/backtest", app.handleBacktest)

	log.Printf("serving on http://localhost%s", *addr)
	if err := http.ListenAndServe(*addr, mux); err != nil {
		log.Fatal(err)
	}
}

func (app *App) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := indexTemplate.Execute(w, nil); err != nil {
		log.Printf("render index: %v", err)
	}
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
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open spot csv: %w", err)
	}
	defer f.Close()

	reader := csv.NewReader(f)
	reader.FieldsPerRecord = -1
	if _, err := reader.Read(); err != nil {
		return nil, fmt.Errorf("read spot header: %w", err)
	}
	spots := make(map[string]float64)
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
		closePrice, err := parseFloat(row[4])
		if err != nil || closePrice <= 0 {
			continue
		}
		spots[strings.TrimSpace(row[0])] = closePrice
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

const indexHTML = `<!doctype html>
<html lang="zh-CN">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>Axiom FutOpt</title>
  <style>
    :root { color-scheme: light; font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
    body { margin: 0; background: #f7f8fa; color: #172033; }
    header { height: 52px; display: flex; align-items: center; gap: 16px; padding: 0 20px; background: #ffffff; border-bottom: 1px solid #dfe3ea; }
    h1 { font-size: 18px; margin: 0; font-weight: 650; }
    main { display: grid; grid-template-columns: minmax(320px, 420px) 1fr; gap: 16px; padding: 16px; }
    section { background: #ffffff; border: 1px solid #dfe3ea; border-radius: 8px; min-width: 0; }
    .panel-head { display: flex; align-items: center; justify-content: space-between; padding: 12px 14px; border-bottom: 1px solid #e7ebf1; }
    .panel-head h2 { font-size: 15px; margin: 0; }
    .controls { display: grid; grid-template-columns: 1fr 96px 96px; gap: 8px; padding: 12px 14px; }
    .backtest-controls { display: grid; grid-template-columns: repeat(6, minmax(110px, 1fr)); gap: 8px; padding: 12px 14px; }
    input, select, button { height: 34px; border: 1px solid #c9d1dc; border-radius: 6px; padding: 0 10px; font: inherit; background: #fff; box-sizing: border-box; }
    button { cursor: pointer; background: #234f8f; color: #fff; border-color: #234f8f; font-weight: 600; }
    button.secondary { background: #ffffff; color: #234f8f; }
    table { width: 100%; border-collapse: collapse; font-size: 12px; }
    th, td { padding: 7px 8px; border-bottom: 1px solid #edf0f4; text-align: right; white-space: nowrap; }
    th:first-child, td:first-child, th:nth-child(2), td:nth-child(2) { text-align: left; }
    thead th { position: sticky; top: 0; background: #f2f5f9; z-index: 1; }
    .table-wrap { max-height: calc(100vh - 210px); overflow: auto; }
    .contracts { max-height: calc(100vh - 168px); overflow: auto; }
    .contract-row { cursor: pointer; }
    .contract-row:hover { background: #eef5ff; }
    .summary { display: grid; grid-template-columns: repeat(5, minmax(120px, 1fr)); gap: 8px; padding: 0 14px 12px; }
    .metric { border: 1px solid #e2e7ef; border-radius: 6px; padding: 8px 10px; background: #fbfcfe; }
    .metric span { display: block; color: #657085; font-size: 12px; }
    .metric strong { display: block; margin-top: 4px; font-size: 16px; }
    .status { color: #657085; font-size: 12px; padding: 0 14px 12px; min-height: 18px; }
    .split { display: grid; grid-template-rows: auto 1fr; gap: 16px; min-width: 0; }
    @media (max-width: 980px) {
      main { grid-template-columns: 1fr; }
      .backtest-controls { grid-template-columns: repeat(2, minmax(120px, 1fr)); }
      .summary { grid-template-columns: repeat(2, minmax(120px, 1fr)); }
    }
  </style>
</head>
<body>
  <header>
    <h1>Axiom FutOpt</h1>
  </header>
  <main>
    <section>
      <div class="panel-head"><h2>合约</h2><button class="secondary" id="refreshContracts">刷新</button></div>
      <div class="controls">
        <input id="contractQuery" value="IM" placeholder="合约代码 / 前缀">
        <select id="kindFilter"><option value="">全部</option><option value="future">期货</option><option value="option">期权</option></select>
        <input id="contractLimit" value="200" inputmode="numeric" title="返回数量">
      </div>
      <div class="status" id="contractStatus"></div>
      <div class="contracts">
        <table>
          <thead><tr><th>代码</th><th>类型</th><th>首日</th><th>末日</th><th>行数</th></tr></thead>
          <tbody id="contractsBody"></tbody>
        </table>
      </div>
    </section>

    <div class="split">
      <section>
        <div class="panel-head"><h2 id="historyTitle">历史</h2><input id="historyLimit" value="500" inputmode="numeric" title="历史行数"></div>
        <div class="status" id="historyStatus"></div>
        <div class="table-wrap">
          <table>
            <thead><tr><th>日期</th><th>合约</th><th>开</th><th>高</th><th>低</th><th>收</th><th>结算</th><th>量</th><th>持仓</th><th>Delta</th></tr></thead>
            <tbody id="historyBody"></tbody>
          </table>
        </div>
      </section>

      <section>
        <div class="panel-head"><h2>IM 吃贴水回测</h2><button id="runBacktest">运行</button></div>
        <div class="backtest-controls">
          <input id="basisYield" value="0.06" title="贴水年化收益率阈值">
          <input id="rollDays" value="5" inputmode="numeric" title="到期日前移仓天数">
          <input id="multiplier" value="200" inputmode="numeric" title="合约乘数">
          <input id="startDate" placeholder="开始 YYYY-MM-DD">
          <input id="endDate" placeholder="结束 YYYY-MM-DD">
          <button id="runBacktest2">运行</button>
        </div>
        <div class="summary" id="backtestSummary"></div>
        <div class="status" id="backtestStatus"></div>
        <div class="table-wrap" style="max-height: 300px;">
          <table>
            <thead><tr><th>日期</th><th>动作</th><th>合约</th><th>价格</th><th>现货</th><th>年化贴水</th><th>到期天数</th><th>累计收益</th></tr></thead>
            <tbody id="eventsBody"></tbody>
          </table>
        </div>
      </section>
    </div>
  </main>

  <script>
    const fmt = new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 4 });
    const money = new Intl.NumberFormat('zh-CN', { maximumFractionDigits: 2 });

    async function api(path) {
      const res = await fetch(path);
      const body = await res.json();
      if (!res.ok) throw new Error(body.error || res.statusText);
      return body;
    }

	function setStatus(id, text) { document.getElementById(id).textContent = text || ''; }
	function esc(value) { return String(value ?? '').replace(/[&<>"]/g, c => ({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;'}[c])); }

    async function loadContracts() {
      const q = document.getElementById('contractQuery').value.trim();
      const kind = document.getElementById('kindFilter').value;
      const limit = document.getElementById('contractLimit').value || '200';
      setStatus('contractStatus', '加载中...');
			const data = await api('/api/contracts?q=' + encodeURIComponent(q) + '&kind=' + encodeURIComponent(kind) + '&limit=' + encodeURIComponent(limit));
      const body = document.getElementById('contractsBody');
			body.innerHTML = data.contracts.map(c =>
				'<tr class="contract-row" data-code="' + esc(c.code) + '">' +
					'<td>' + esc(c.code) + '</td><td>' + esc(c.kind) + '</td><td>' + esc(c.first_date) + '</td><td>' + esc(c.last_date) + '</td><td>' + esc(c.rows) + '</td>' +
				'</tr>').join('');
      body.querySelectorAll('tr').forEach(row => row.addEventListener('click', () => loadHistory(row.dataset.code)));
			setStatus('contractStatus', data.count + ' 个合约');
      if (data.contracts.length > 0) loadHistory(data.contracts[0].code);
    }

    async function loadHistory(code) {
      const limit = document.getElementById('historyLimit').value || '500';
      setStatus('historyStatus', '加载中...');
			const data = await api('/api/history?code=' + encodeURIComponent(code) + '&limit=' + encodeURIComponent(limit));
			document.getElementById('historyTitle').textContent = data.contract.code + ' 历史';
			document.getElementById('historyBody').innerHTML = data.records.map(r =>
				'<tr><td>' + esc(r.date) + '</td><td>' + esc(r.code) + '</td><td>' + esc(r.open) + '</td><td>' + esc(r.high) + '</td><td>' + esc(r.low) + '</td><td>' + esc(r.close) + '</td><td>' + esc(r.settle) + '</td><td>' + esc(r.volume) + '</td><td>' + esc(r.open_interest) + '</td><td>' + esc(r.delta) + '</td></tr>').join('');
			setStatus('historyStatus', data.count + ' 行，区间 ' + data.contract.first_date + ' 到 ' + data.contract.last_date);
    }

    async function runBacktest() {
      const params = new URLSearchParams({
        basis_yield: document.getElementById('basisYield').value || '0.06',
        roll_days: document.getElementById('rollDays').value || '5',
        multiplier: document.getElementById('multiplier').value || '200',
        start: document.getElementById('startDate').value,
        end: document.getElementById('endDate').value,
      });
      setStatus('backtestStatus', '运行中...');
      const data = await api('/api/backtest?' + params.toString());
      document.getElementById('backtestSummary').innerHTML = [
        ['总收益', money.format(data.total_profit)],
        ['持有天数', data.holding_days],
        ['移仓次数', data.rolls],
        ['最终合约', data.final_contract],
        ['最终结算', fmt.format(data.final_settle)],
			].map(([k, v]) => '<div class="metric"><span>' + k + '</span><strong>' + v + '</strong></div>').join('');
			document.getElementById('eventsBody').innerHTML = data.events.map(e =>
				'<tr><td>' + esc(e.date) + '</td><td>' + esc(e.action) + '</td><td>' + esc(e.code) + '</td><td>' + fmt.format(e.price) + '</td><td>' + fmt.format(e.spot_close) + '</td><td>' + fmt.format(e.annualized_basis * 100) + '%</td><td>' + e.days_to_expiry + '</td><td>' + money.format(e.cumulative_profit) + '</td></tr>').join('');
			setStatus('backtestStatus', data.start_date + ' 到 ' + data.end_date);
    }

    document.getElementById('refreshContracts').addEventListener('click', () => loadContracts().catch(e => setStatus('contractStatus', e.message)));
    document.getElementById('contractQuery').addEventListener('keydown', e => { if (e.key === 'Enter') loadContracts().catch(err => setStatus('contractStatus', err.message)); });
    document.getElementById('kindFilter').addEventListener('change', () => loadContracts().catch(e => setStatus('contractStatus', e.message)));
    document.getElementById('historyLimit').addEventListener('keydown', e => { if (e.key === 'Enter') loadContracts().catch(err => setStatus('historyStatus', err.message)); });
    document.getElementById('runBacktest').addEventListener('click', () => runBacktest().catch(e => setStatus('backtestStatus', e.message)));
    document.getElementById('runBacktest2').addEventListener('click', () => runBacktest().catch(e => setStatus('backtestStatus', e.message)));

    loadContracts().catch(e => setStatus('contractStatus', e.message));
  </script>
</body>
</html>`
