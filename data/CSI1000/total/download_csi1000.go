package main

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	eastmoneyKlineURL = "https://push2his.eastmoney.com/api/qt/stock/kline/get?secid=1.000852&fields1=f1,f2,f3,f4,f5,f6&fields2=f51,f52,f53,f54,f55&klt=101&fqt=0&beg=20040101&end=20500101"
	outputFileName    = "csi1000_daily.csv"
	requestTimeout    = 30 * time.Second
	atrPeriod         = 14
)

type eastmoneyKlineResponse struct {
	Data *struct {
		Code   string   `json:"code"`
		Name   string   `json:"name"`
		Klines []string `json:"klines"`
	} `json:"data"`
}

type dailyCSVRow struct {
	TradeDate string
	Open      string
	High      string
	Low       string
	Close     string
	ATR       string
	ATRPct    string
}

func main() {
	ctx, cancel := context.WithTimeout(context.Background(), requestTimeout)
	defer cancel()

	outputPath, err := resolveOutputPath()
	if err != nil {
		fmt.Fprintf(os.Stderr, "resolve output path: %v\n", err)
		os.Exit(1)
	}

	rows, err := fetchCSI1000Rows(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "fetch CSI 1000 history: %v\n", err)
		os.Exit(1)
	}

	if err := os.MkdirAll(filepath.Dir(outputPath), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "create output dir: %v\n", err)
		os.Exit(1)
	}

	file, err := os.Create(outputPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "create %s: %v\n", outputPath, err)
		os.Exit(1)
	}
	defer file.Close()

	writer := csv.NewWriter(file)
	defer writer.Flush()

	if err := writer.Write([]string{"trade_date", "open", "high", "low", "close", "atr", "atr_pct"}); err != nil {
		fmt.Fprintf(os.Stderr, "write csv header: %v\n", err)
		os.Exit(1)
	}

	for _, row := range rows {
		if err := writer.Write([]string{row.TradeDate, row.Open, row.High, row.Low, row.Close, row.ATR, row.ATRPct}); err != nil {
			fmt.Fprintf(os.Stderr, "write csv row: %v\n", err)
			os.Exit(1)
		}
	}

	if err := writer.Error(); err != nil {
		fmt.Fprintf(os.Stderr, "flush csv writer: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("saved %d rows to %s\n", len(rows), outputPath)
}

func resolveOutputPath() (string, error) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		return "", fmt.Errorf("cannot resolve current file path")
	}
	return filepath.Join(filepath.Dir(currentFile), outputFileName), nil
}

func fetchCSI1000Rows(ctx context.Context) ([]dailyCSVRow, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, eastmoneyKlineURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0")
	req.Header.Set("Referer", "https://quote.eastmoney.com")

	resp, err := (&http.Client{Timeout: requestTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status: %s", resp.Status)
	}

	var payload eastmoneyKlineResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, err
	}
	if payload.Data == nil || len(payload.Data.Klines) == 0 {
		return nil, fmt.Errorf("empty Eastmoney kline payload")
	}

	rows := make([]dailyCSVRow, 0, len(payload.Data.Klines))
	for _, line := range payload.Data.Klines {
		parts := strings.Split(line, ",")
		if len(parts) < 5 {
			return nil, fmt.Errorf("unexpected kline format: %s", line)
		}
		rows = append(rows, dailyCSVRow{
			TradeDate: parts[0],
			Open:      parts[1],
			High:      parts[3],
			Low:       parts[4],
			Close:     parts[2],
		})
	}

	if err := appendATR(rows); err != nil {
		return nil, err
	}

	return rows, nil
}

func appendATR(rows []dailyCSVRow) error {
	if len(rows) == 0 {
		return nil
	}

	trs := make([]float64, len(rows))
	prevClose := 0.0
	for idx := range rows {
		high, err := strconv.ParseFloat(rows[idx].High, 64)
		if err != nil {
			return fmt.Errorf("parse high %q: %w", rows[idx].High, err)
		}
		low, err := strconv.ParseFloat(rows[idx].Low, 64)
		if err != nil {
			return fmt.Errorf("parse low %q: %w", rows[idx].Low, err)
		}
		closePrice, err := strconv.ParseFloat(rows[idx].Close, 64)
		if err != nil {
			return fmt.Errorf("parse close %q: %w", rows[idx].Close, err)
		}

		tr := high - low
		if idx > 0 {
			upGap := absFloat(high - prevClose)
			downGap := absFloat(low - prevClose)
			if upGap > tr {
				tr = upGap
			}
			if downGap > tr {
				tr = downGap
			}
		}
		trs[idx] = tr
		prevClose = closePrice
	}

	if len(rows) < atrPeriod {
		return nil
	}

	atr := 0.0
	for idx := 0; idx < atrPeriod; idx++ {
		atr += trs[idx]
	}
	atr /= float64(atrPeriod)
	if err := fillATR(&rows[atrPeriod-1], atr); err != nil {
		return err
	}

	for idx := atrPeriod; idx < len(rows); idx++ {
		atr = ((atr * float64(atrPeriod-1)) + trs[idx]) / float64(atrPeriod)
		if err := fillATR(&rows[idx], atr); err != nil {
			return err
		}
	}

	return nil
}

func fillATR(row *dailyCSVRow, atr float64) error {
	closePrice, err := strconv.ParseFloat(row.Close, 64)
	if err != nil {
		return fmt.Errorf("parse close %q: %w", row.Close, err)
	}
	row.ATR = fmt.Sprintf("%.4f", atr)
	if closePrice > 0 {
		row.ATRPct = fmt.Sprintf("%.4f", atr/closePrice*100)
	}
	return nil
}

func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}