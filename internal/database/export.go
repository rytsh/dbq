package database

import (
	"context"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
)

const (
	// DefaultExportBatchSize is the number of rows emitted per INSERT statement.
	DefaultExportBatchSize = 100
	// MaxExportBatchSize bounds memory use and SQL statement size.
	MaxExportBatchSize = 1000
	// DefaultMaxExportRows bounds MCP exports when no explicit operator limit is set.
	DefaultMaxExportRows = 100000
)

// ExportOptions controls a streaming INSERT statement export.
type ExportOptions struct {
	Dialect   string
	Table     string
	BatchSize int
	MaxRows   int
	Timeout   time.Duration
}

// ExportResult describes an INSERT export without containing its data.
type ExportResult struct {
	Rows       int
	Statements int
	Columns    int
}

var exportDialects = []string{
	"godror", "ingres", "mariadb", "mssql", "mysql", "odbc", "oracle", "pgx",
	"postgres", "postgresql", "sqlite", "sqlite3", "sqlserver",
}

// NormalizeExportDialect validates a target dialect and returns its canonical name.
func NormalizeExportDialect(dialect string) (string, error) {
	dialect = strings.ToLower(strings.TrimSpace(dialect))
	if !slices.Contains(exportDialects, dialect) {
		return "", fmt.Errorf("unsupported export dialect %q", dialect)
	}

	switch dialect {
	case "pgx", "postgres":
		return "postgresql", nil
	case "sqlite":
		return "sqlite3", nil
	case "mssql":
		return "sqlserver", nil
	case "godror":
		return "oracle", nil
	case "mariadb":
		return "mysql", nil
	default:
		return dialect, nil
	}
}

// ExportInserts runs a read query and writes its rows as batched INSERT statements.
func ExportInserts(ctx context.Context, db *sqlx.DB, query string, w io.Writer, opts ExportOptions) (*ExportResult, error) {
	if err := normalizeExportOptions(&opts); err != nil {
		return nil, err
	}

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	rows, err := db.QueryxContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("querying database for export, err: %w", err)
	}
	defer func() { _ = rows.Close() }()

	columns, columnTypes, err := exportColumns(rows)
	if err != nil {
		return nil, err
	}

	result := &ExportResult{Columns: len(columns)}
	batch := make([][]any, 0, opts.BatchSize)

	flush := func() error {
		if len(batch) == 0 {
			return nil
		}

		if err := writeInsert(w, opts.Dialect, opts.Table, columns, batch); err != nil {
			return err
		}

		result.Statements++
		batch = batch[:0]

		return nil
	}

	for rows.Next() {
		if opts.MaxRows > 0 && result.Rows >= opts.MaxRows {
			return nil, fmt.Errorf("export exceeds the %d row limit", opts.MaxRows)
		}

		values, err := rows.SliceScan()
		if err != nil {
			return nil, fmt.Errorf("scanning export row, err: %w", err)
		}
		if err := normalizeExportRow(values, columnTypes); err != nil {
			return nil, err
		}

		batch = append(batch, values)
		result.Rows++

		if len(batch) == opts.BatchSize {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating export rows, err: %w", err)
	}

	if err := flush(); err != nil {
		return nil, err
	}

	return result, nil
}

func normalizeExportOptions(opts *ExportOptions) error {
	if opts.Table == "" {
		return fmt.Errorf("target table is required")
	}
	for _, component := range strings.Split(opts.Table, ".") {
		if strings.TrimSpace(component) == "" {
			return fmt.Errorf("target table contains an empty identifier")
		}
	}

	if opts.BatchSize <= 0 {
		opts.BatchSize = DefaultExportBatchSize
	}
	if opts.BatchSize > MaxExportBatchSize {
		return fmt.Errorf("batch size cannot exceed %d", MaxExportBatchSize)
	}

	return nil
}

func exportColumns(rows *sqlx.Rows) ([]string, []*sql.ColumnType, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, nil, fmt.Errorf("reading export columns, err: %w", err)
	}
	columnTypes, err := rows.ColumnTypes()
	if err != nil {
		return nil, nil, fmt.Errorf("reading export column types, err: %w", err)
	}

	seen := make(map[string]struct{}, len(columns))
	for _, column := range columns {
		key := strings.ToLower(column)
		if _, duplicate := seen[key]; duplicate {
			return nil, nil, fmt.Errorf("duplicate export column %q; give duplicate columns unique aliases", column)
		}
		seen[key] = struct{}{}
	}

	return columns, columnTypes, nil
}

func normalizeExportRow(values []any, columnTypes []*sql.ColumnType) error {
	for i, value := range values {
		if number, ok := value.(float64); ok && (math.IsNaN(number) || math.IsInf(number, 0)) {
			return fmt.Errorf("column %q contains non-finite floating-point value", columnTypes[i].Name())
		}
		if number, ok := value.(float32); ok && (math.IsNaN(float64(number)) || math.IsInf(float64(number), 0)) {
			return fmt.Errorf("column %q contains non-finite floating-point value", columnTypes[i].Name())
		}

		bytes, ok := value.([]byte)
		if !ok {
			continue
		}

		if isBinaryType(columnTypes[i].DatabaseTypeName()) || !utf8.Valid(bytes) {
			values[i] = binaryValue(bytes)
		} else {
			values[i] = string(bytes)
		}
	}

	return nil
}

func writeInsert(w io.Writer, dialect, table string, columns []string, rows [][]any) error {
	quotedColumns := make([]string, len(columns))
	for i, column := range columns {
		quotedColumns[i] = quoteIdentifier(dialect, column)
	}

	target := quoteQualifiedIdentifier(dialect, table)
	columnList := strings.Join(quotedColumns, ", ")

	if isOracle(dialect) && len(rows) > 1 {
		if _, err := io.WriteString(w, "INSERT ALL\n"); err != nil {
			return err
		}

		for _, row := range rows {
			if _, err := fmt.Fprintf(w, "  INTO %s (%s) VALUES (%s)\n", target, columnList, valueList(dialect, row)); err != nil {
				return err
			}
		}

		_, err := io.WriteString(w, "SELECT 1 FROM DUAL;\n")

		return err
	}

	if _, err := fmt.Fprintf(w, "INSERT INTO %s (%s) VALUES\n", target, columnList); err != nil {
		return err
	}

	for i, row := range rows {
		ending := ",\n"
		if i == len(rows)-1 {
			ending = ";\n"
		}

		if _, err := fmt.Fprintf(w, "  (%s)%s", valueList(dialect, row), ending); err != nil {
			return err
		}
	}

	return nil
}

func valueList(dialect string, row []any) string {
	values := make([]string, len(row))
	for i, value := range row {
		values[i] = sqlLiteral(dialect, value)
	}

	return strings.Join(values, ", ")
}

func sqlLiteral(dialect string, value any) string {
	switch v := value.(type) {
	case nil:
		return "NULL"
	case bool:
		if !isSQLServer(dialect) && !isOracle(dialect) {
			return strings.ToUpper(strconv.FormatBool(v))
		}
		if v {
			return "1"
		}

		return "0"
	case binaryValue:
		encoded := strings.ToUpper(hex.EncodeToString(v))
		if isPostgres(dialect) {
			return "decode('" + encoded + "', 'hex')"
		}
		if isOracle(dialect) {
			return "HEXTORAW('" + encoded + "')"
		}
		if isSQLServer(dialect) {
			return "0x" + encoded
		}

		return "X'" + encoded + "'"
	case time.Time:
		return sqlStringLiteral(dialect, v.Format(time.RFC3339Nano))
	case string:
		return sqlStringLiteral(dialect, v)
	case float32:
		return strconv.FormatFloat(float64(v), 'g', -1, 32)
	case float64:
		return strconv.FormatFloat(v, 'g', -1, 64)
	case int:
		return strconv.Itoa(v)
	case int8, int16, int32, int64:
		return fmt.Sprintf("%d", v)
	case uint, uint8, uint16, uint32, uint64:
		return fmt.Sprintf("%d", v)
	default:
		return sqlStringLiteral(dialect, fmt.Sprint(v))
	}
}

type binaryValue []byte

func sqlStringLiteral(dialect, value string) string {
	switch strings.ToLower(dialect) {
	case "mysql":
		return "CONVERT(X'" + strings.ToUpper(hex.EncodeToString([]byte(value))) + "' USING utf8mb4)"
	case "postgresql":
		return "convert_from(decode('" + strings.ToUpper(hex.EncodeToString([]byte(value))) + "', 'hex'), 'UTF8')"
	case "sqlserver":
		return "N" + quoteString(value)
	default:
		return quoteString(value)
	}
}

func isBinaryType(databaseType string) bool {
	databaseType = strings.ToUpper(databaseType)
	for _, marker := range []string{"BINARY", "BLOB", "BYTEA", "IMAGE", "RAW"} {
		if strings.Contains(databaseType, marker) {
			return true
		}
	}

	return false
}

func quoteString(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func quoteQualifiedIdentifier(dialect, name string) string {
	parts := strings.Split(name, ".")
	for i, part := range parts {
		parts[i] = quoteIdentifier(dialect, part)
	}

	return strings.Join(parts, ".")
}

func quoteIdentifier(dialect, name string) string {
	switch strings.ToLower(dialect) {
	case "mysql", "mariadb":
		return "`" + strings.ReplaceAll(name, "`", "``") + "`"
	case "sqlserver", "mssql":
		return "[" + strings.ReplaceAll(name, "]", "]]") + "]"
	default:
		return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
	}
}

func isPostgres(dialect string) bool {
	dialect = strings.ToLower(dialect)

	return dialect == "pgx" || dialect == "postgres" || dialect == "postgresql"
}

func isOracle(dialect string) bool {
	dialect = strings.ToLower(dialect)

	return dialect == "godror" || dialect == "oracle"
}

func isSQLServer(dialect string) bool {
	dialect = strings.ToLower(dialect)

	return dialect == "sqlserver" || dialect == "mssql"
}
