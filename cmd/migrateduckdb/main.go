package main

import (
	"database/sql"
	"encoding/csv"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
)

const dateLayout = "20060102"

var contractCodePattern = regexp.MustCompile(`^([A-Z]{1,3})(\d{4})(?:-([CP])-(\d+(?:\.\d+)?))?$`)

type migrationConfig struct {
	ExtractedDir string
	DuckDBPath   string
	StartDate    string
	FullHistory  bool
}

type marketRow struct {
	Date         string
	Code         string
	Open         sql.NullFloat64
	High         sql.NullFloat64
	Low          sql.NullFloat64
	Volume       sql.NullInt64
	Amount       sql.NullFloat64
	OpenInterest sql.NullInt64
	OIChange     sql.NullInt64
	Close        sql.NullFloat64
	Settle       sql.NullFloat64
	PrevSettle   sql.NullFloat64
	Change1      sql.NullFloat64
	Change2      sql.NullFloat64
	Delta        sql.NullFloat64
}

func main() {
	config := migrationConfig{}
	flag.StringVar(&config.ExtractedDir, "extracted-dir", "extracted", "directory containing daily CFFEX CSV files")
	flag.StringVar(&config.DuckDBPath, "duckdb", "data/duckdb/market.duckdb", "target DuckDB database file")
	flag.StringVar(&config.StartDate, "start-date", "", "optional start date for incremental migration, format YYYYMMDD")
	flag.BoolVar(&config.FullHistory, "full-history", false, "migrate all historical data")
	flag.Parse()

	if err := migrate(config); err != nil {
		exitErr(err)
	}

	fmt.Printf("migration program ready: source=%s target=%s\n", config.ExtractedDir, config.DuckDBPath)
}

func migrate(config migrationConfig) error {
	startDate, err := resolveStartDate(config)
	if err != nil {
		return err
	}

	db, err := sql.Open("duckdb", config.DuckDBPath)
	if err != nil {
		return fmt.Errorf("open duckdb: %w", err)
	}
	defer db.Close()

	if err := createTables(db); err != nil {
		return err
	}

	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	if err := clearTargetRange(tx, startDate); err != nil {
		return err
	}

	futuresStmt, err := tx.Prepare(insertSQL("futures_daily"))
	if err != nil {
		return fmt.Errorf("prepare futures insert: %w", err)
	}
	defer futuresStmt.Close()

	optionsStmt, err := tx.Prepare(insertSQL("options_daily"))
	if err != nil {
		return fmt.Errorf("prepare options insert: %w", err)
	}
	defer optionsStmt.Close()

	err = filepath.WalkDir(config.ExtractedDir, func(path string, entry fs.DirEntry, walkErr error) error {
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
		if startDate != "" && date < startDate {
			return nil
		}

		return importFile(path, date, futuresStmt, optionsStmt)
	})
	if err != nil {
		return fmt.Errorf("walk extracted dir: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	tx = nil
	return nil
}

func resolveStartDate(config migrationConfig) (string, error) {
	if config.FullHistory {
		if strings.TrimSpace(config.StartDate) != "" {
			return "", errors.New("start-date and full-history cannot be used together")
		}
		return "", nil
	}

	value := strings.TrimSpace(config.StartDate)
	if value == "" {
		return time.Now().In(time.Local).Format(dateLayout), nil
	}
	if _, err := time.ParseInLocation(dateLayout, value, time.Local); err != nil {
		return "", fmt.Errorf("invalid start-date %q, want YYYYMMDD", config.StartDate)
	}
	return value, nil
}

func clearTargetRange(tx *sql.Tx, startDate string) error {
	if startDate == "" {
		if _, err := tx.Exec(`DELETE FROM futures_daily`); err != nil {
			return fmt.Errorf("clear futures_daily: %w", err)
		}
		if _, err := tx.Exec(`DELETE FROM options_daily`); err != nil {
			return fmt.Errorf("clear options_daily: %w", err)
		}
		return nil
	}

	if _, err := tx.Exec(`DELETE FROM futures_daily WHERE trade_date >= ?`, startDate); err != nil {
		return fmt.Errorf("clear futures_daily from %s: %w", startDate, err)
	}
	if _, err := tx.Exec(`DELETE FROM options_daily WHERE trade_date >= ?`, startDate); err != nil {
		return fmt.Errorf("clear options_daily from %s: %w", startDate, err)
	}
	return nil
}

func createTables(db *sql.DB) error {
	const schema = `
CREATE TABLE IF NOT EXISTS futures_daily (
	trade_date VARCHAR NOT NULL,
	contract_code VARCHAR NOT NULL,
	open DOUBLE,
	high DOUBLE,
	low DOUBLE,
	volume BIGINT,
	amount DOUBLE,
	open_interest BIGINT,
	oi_change BIGINT,
	close DOUBLE,
	settle DOUBLE,
	prev_settle DOUBLE,
	change1 DOUBLE,
	change2 DOUBLE,
	delta DOUBLE
);

CREATE TABLE IF NOT EXISTS options_daily (
	trade_date VARCHAR NOT NULL,
	contract_code VARCHAR NOT NULL,
	open DOUBLE,
	high DOUBLE,
	low DOUBLE,
	volume BIGINT,
	amount DOUBLE,
	open_interest BIGINT,
	oi_change BIGINT,
	close DOUBLE,
	settle DOUBLE,
	prev_settle DOUBLE,
	change1 DOUBLE,
	change2 DOUBLE,
	delta DOUBLE
);
`

	if _, err := db.Exec(schema); err != nil {
		return fmt.Errorf("create tables: %w", err)
	}
	return nil
}

func insertSQL(table string) string {
	return fmt.Sprintf(`
INSERT INTO %s (
	trade_date,
	contract_code,
	open,
	high,
	low,
	volume,
	amount,
	open_interest,
	oi_change,
	close,
	settle,
	prev_settle,
	change1,
	change2,
	delta
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, table)
}

func importFile(path string, date string, futuresStmt, optionsStmt *sql.Stmt) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	reader := csv.NewReader(file)
	reader.FieldsPerRecord = -1
	if _, err := reader.Read(); err != nil {
		return fmt.Errorf("read header %s: %w", path, err)
	}

	for {
		record, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read row %s: %w", path, err)
		}

		row, kind, ok := parseMarketRow(date, record)
		if !ok {
			continue
		}

		stmt := futuresStmt
		if kind == "option" {
			stmt = optionsStmt
		}
		if _, err := stmt.Exec(
			row.Date,
			row.Code,
			row.Open,
			row.High,
			row.Low,
			row.Volume,
			row.Amount,
			row.OpenInterest,
			row.OIChange,
			row.Close,
			row.Settle,
			row.PrevSettle,
			row.Change1,
			row.Change2,
			row.Delta,
		); err != nil {
			return fmt.Errorf("insert row %s %s: %w", path, row.Code, err)
		}
	}
}

func parseMarketRow(date string, record []string) (marketRow, string, bool) {
	if len(record) < 14 {
		return marketRow{}, "", false
	}

	code := normalizeCode(record[0])
	kind, ok := contractKind(code)
	if !ok {
		return marketRow{}, "", false
	}

	return marketRow{
		Date:         date,
		Code:         code,
		Open:         parseNullFloat(field(record, 1)),
		High:         parseNullFloat(field(record, 2)),
		Low:          parseNullFloat(field(record, 3)),
		Volume:       parseNullInt(field(record, 4)),
		Amount:       parseNullFloat(field(record, 5)),
		OpenInterest: parseNullInt(field(record, 6)),
		OIChange:     parseNullInt(field(record, 7)),
		Close:        parseNullFloat(field(record, 8)),
		Settle:       parseNullFloat(field(record, 9)),
		PrevSettle:   parseNullFloat(field(record, 10)),
		Change1:      parseNullFloat(field(record, 11)),
		Change2:      parseNullFloat(field(record, 12)),
		Delta:        parseNullFloat(field(record, 13)),
	}, kind, true
}

func normalizeCode(raw string) string {
	value := strings.TrimSpace(strings.TrimPrefix(raw, "\ufeff"))
	return strings.ToUpper(value)
}

func contractKind(code string) (string, bool) {
	matches := contractCodePattern.FindStringSubmatch(code)
	if matches == nil {
		return "", false
	}
	if matches[3] == "" {
		return "future", true
	}
	return "option", true
}

func field(record []string, index int) string {
	if index >= len(record) {
		return ""
	}
	return strings.TrimSpace(record[index])
}

func parseNullFloat(raw string) sql.NullFloat64 {
	value := strings.TrimSpace(raw)
	if value == "" || value == "--" {
		return sql.NullFloat64{}
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return sql.NullFloat64{}
	}
	return sql.NullFloat64{Float64: parsed, Valid: true}
}

func parseNullInt(raw string) sql.NullInt64 {
	parsed := parseNullFloat(raw)
	if !parsed.Valid {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: int64(parsed.Float64), Valid: true}
}

func dateFromFileName(name string) (string, bool) {
	if len(name) < 8 {
		return "", false
	}
	date := name[:8]
	if _, err := time.ParseInLocation(dateLayout, date, time.Local); err != nil {
		return "", false
	}
	return date, true
}

func exitErr(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
