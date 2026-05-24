package main

// 逻辑：

// 只交易 IM，长期持有 1 张中证1000期货
// 用 今结算 算每日盈亏
// 年化贴水 = (现货收盘 / 期货结算 - 1) * 365 / 剩余到期天数
// 到期日按中金所规则：合约月第三个周五；若数据里该日非交易日，顺延到下一交易日
// 当前合约距离到期 <= roll-days 时移仓
// 新合约优先选年化贴水 >= basis-yield，再选年化贴水最高者

import (
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const dayLayout = "2006-01-02"

var imCodePattern = regexp.MustCompile(`^IM(\d{2})(\d{2})$`)

type FuturesBar struct {
	Date         time.Time
	Code         string
	Settle       float64
	Expiry       time.Time
	Volume       int64
	OpenInterest int64
}

type Candidate struct {
	Bar             FuturesBar
	AnnualizedBasis float64
	DaysToExpiry    int
	MeetsThreshold  bool
}

type TradeEvent struct {
	Date             time.Time
	Action           string
	Code             string
	Price            float64
	SpotClose        float64
	AnnualizedBasis  float64
	DaysToExpiry     int
	CumulativeProfit float64
}

type Result struct {
	StartDate   time.Time
	EndDate     time.Time
	Days        int
	Entries     int
	Rolls       int
	FinalCode   string
	FinalSettle float64
	Profit      float64
	Events      []TradeEvent
}

func main() {
	futuresDir := flag.String("futures-dir", "extracted", "directory containing CFFEX daily CSV files")
	spotCSV := flag.String("spot-csv", "data/csi1000_000852_daily_ohlc_since_launch.csv", "CSI 1000 daily OHLC CSV")
	basisYield := flag.Float64("basis-yield", 0.06, "minimum annualized discount yield, e.g. 0.06 means 6%")
	rollDays := flag.Int("roll-days", 5, "roll when current contract has this many or fewer calendar days before expiry")
	multiplier := flag.Float64("multiplier", 200, "IM futures multiplier in CNY per index point")
	startRaw := flag.String("start", "", "optional start date, YYYY-MM-DD")
	endRaw := flag.String("end", "", "optional end date, YYYY-MM-DD")
	tradesCSV := flag.String("trades-csv", "", "optional output CSV for entry and roll events")
	flag.Parse()

	if *rollDays < 0 {
		exitErr(errors.New("roll-days must be >= 0"))
	}
	if *multiplier <= 0 {
		exitErr(errors.New("multiplier must be > 0"))
	}

	start, err := optionalDate(*startRaw)
	if err != nil {
		exitErr(err)
	}
	end, err := optionalDate(*endRaw)
	if err != nil {
		exitErr(err)
	}

	spots, err := loadSpotClose(*spotCSV)
	if err != nil {
		exitErr(err)
	}
	futures, tradingDays, err := loadIMFutures(*futuresDir)
	if err != nil {
		exitErr(err)
	}
	adjustExpiries(futures, tradingDays)

	result, err := backtest(futures, spots, *basisYield, *rollDays, *multiplier, start, end)
	if err != nil {
		exitErr(err)
	}
	if *tradesCSV != "" {
		if err := writeEvents(*tradesCSV, result.Events); err != nil {
			exitErr(err)
		}
	}

	fmt.Printf("basis_yield_threshold=%.6f\n", *basisYield)
	fmt.Printf("roll_days_before_expiry=%d\n", *rollDays)
	fmt.Printf("multiplier=%.2f\n", *multiplier)
	fmt.Printf("start_date=%s\n", result.StartDate.Format(dayLayout))
	fmt.Printf("end_date=%s\n", result.EndDate.Format(dayLayout))
	fmt.Printf("holding_days=%d\n", result.Days)
	fmt.Printf("entries=%d\n", result.Entries)
	fmt.Printf("rolls=%d\n", result.Rolls)
	fmt.Printf("final_contract=%s\n", result.FinalCode)
	fmt.Printf("final_settle=%.4f\n", result.FinalSettle)
	fmt.Printf("total_profit=%.2f\n", result.Profit)
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

func loadSpotClose(path string) (map[string]float64, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open spot csv: %w", err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	if _, err := reader.Read(); err != nil {
		return nil, fmt.Errorf("read spot header: %w", err)
	}

	spots := make(map[string]float64)
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read spot row: %w", err)
		}
		if len(record) < 5 {
			continue
		}
		date := strings.TrimSpace(record[0])
		closePrice, err := parseFloat(record[4])
		if err != nil || closePrice <= 0 {
			continue
		}
		spots[date] = closePrice
	}
	if len(spots) == 0 {
		return nil, errors.New("spot csv has no valid rows")
	}
	return spots, nil
}

func loadIMFutures(root string) (map[string][]FuturesBar, []time.Time, error) {
	byDate := make(map[string][]FuturesBar)
	dateSet := make(map[string]time.Time)

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
		bars, err := readIMFile(path, date)
		if err != nil {
			return err
		}
		if len(bars) == 0 {
			return nil
		}
		key := date.Format(dayLayout)
		byDate[key] = append(byDate[key], bars...)
		dateSet[key] = date
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("walk futures dir: %w", err)
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

func readIMFile(path string, date time.Time) ([]FuturesBar, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	if _, err := reader.Read(); err != nil {
		return nil, fmt.Errorf("read header %s: %w", path, err)
	}

	bars := make([]FuturesBar, 0, 4)
	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read row %s: %w", path, err)
		}
		if len(record) < 10 {
			continue
		}
		code := strings.TrimSpace(strings.TrimPrefix(record[0], "\ufeff"))
		if !imCodePattern.MatchString(code) {
			continue
		}
		settle, err := parseFloat(record[9])
		if err != nil || settle <= 0 {
			continue
		}
		expiry, err := expiryFromCode(code)
		if err != nil {
			return nil, err
		}
		bars = append(bars, FuturesBar{
			Date:         date,
			Code:         code,
			Settle:       settle,
			Expiry:       expiry,
			Volume:       parseInt(record[4]),
			OpenInterest: parseInt(record[6]),
		})
	}
	return bars, nil
}

func backtest(futures map[string][]FuturesBar, spots map[string]float64, threshold float64, rollDays int, multiplier float64, start, end time.Time) (Result, error) {
	dates := sortedDates(futures)
	var result Result
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
		byCode := make(map[string]FuturesBar, len(bars))
		for _, bar := range bars {
			byCode[bar.Code] = bar
		}

		needRoll := currentCode == ""
		if currentCode != "" {
			bar, ok := byCode[currentCode]
			if ok {
				if previousSettle > 0 {
					result.Profit += (bar.Settle - previousSettle) * multiplier
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
			if result.StartDate.IsZero() {
				result.StartDate = date
			}
			result.Events = append(result.Events, TradeEvent{
				Date:             date,
				Action:           action,
				Code:             candidate.Bar.Code,
				Price:            candidate.Bar.Settle,
				SpotClose:        spotClose,
				AnnualizedBasis:  candidate.AnnualizedBasis,
				DaysToExpiry:     candidate.DaysToExpiry,
				CumulativeProfit: result.Profit,
			})
		}

		if currentCode != "" {
			result.Days++
			result.EndDate = date
			result.FinalCode = currentCode
			result.FinalSettle = previousSettle
		}
	}
	if currentCode == "" {
		return Result{}, errors.New("no position opened; check data and date range")
	}
	return result, nil
}

func chooseContract(bars []FuturesBar, spotClose float64, date time.Time, threshold float64, rollDays int) (Candidate, bool) {
	candidates := make([]Candidate, 0, len(bars))
	for _, bar := range bars {
		days := daysBetween(date, bar.Expiry)
		if days <= rollDays || bar.Settle <= 0 || spotClose <= 0 {
			continue
		}
		yield := annualizedDiscountYield(spotClose, bar.Settle, days)
		candidates = append(candidates, Candidate{
			Bar:             bar,
			AnnualizedBasis: yield,
			DaysToExpiry:    days,
			MeetsThreshold:  yield >= threshold,
		})
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

func expiryFromCode(code string) (time.Time, error) {
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

func adjustExpiries(futures map[string][]FuturesBar, tradingDays []time.Time) {
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
	idx := sort.Search(len(tradingDays), func(i int) bool {
		return !tradingDays[i].Before(date)
	})
	if idx == len(tradingDays) {
		return date
	}
	return tradingDays[idx]
}

func sortedDates(futures map[string][]FuturesBar) []time.Time {
	dates := make([]time.Time, 0, len(futures))
	for key := range futures {
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

func writeEvents(path string, events []TradeEvent) error {
	dir := filepath.Dir(path)
	if dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("create trades csv: %w", err)
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()
	if err := writer.Write([]string{"date", "action", "code", "price", "spot_close", "annualized_basis", "days_to_expiry", "cumulative_profit"}); err != nil {
		return err
	}
	for _, event := range events {
		record := []string{
			event.Date.Format(dayLayout),
			event.Action,
			event.Code,
			formatFloat(event.Price),
			formatFloat(event.SpotClose),
			formatFloat(event.AnnualizedBasis),
			strconv.Itoa(event.DaysToExpiry),
			formatFloat(event.CumulativeProfit),
		}
		if err := writer.Write(record); err != nil {
			return err
		}
	}
	return writer.Error()
}

func formatFloat(value float64) string {
	return strconv.FormatFloat(value, 'f', 8, 64)
}

func exitErr(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
