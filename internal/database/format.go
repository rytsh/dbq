package database

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cast"
)

// Format is a CLI output rendering of a Result.
type Format string

const (
	// FormatTable is the human-readable ASCII table used by the REPL.
	FormatTable Format = "table"
	// FormatJSON renders a query result as an array of row objects, and an
	// exec result as a single object, for piping into jq.
	FormatJSON Format = "json"
	// FormatCSV renders a header line and one line per row.
	FormatCSV Format = "csv"
)

// Formats lists the accepted output formats.
var Formats = []Format{FormatTable, FormatJSON, FormatCSV}

// ParseFormat normalizes a user-supplied format name.
func ParseFormat(s string) (Format, error) {
	f := Format(strings.ToLower(strings.TrimSpace(s)))

	for _, known := range Formats {
		if f == known {
			return f, nil
		}
	}

	return "", fmt.Errorf("unknown output format %q, want one of: %v", s, Formats)
}

// Write renders a Result in the given format.
//
// Table output includes the truncation notices; JSON and CSV are kept clean so
// they can be consumed by another program, and callers report truncation on
// stderr instead.
func Write(w io.Writer, res *Result, format Format) error {
	switch format {
	case FormatJSON:
		return writeJSON(w, res)
	case FormatCSV:
		return writeCSV(w, res)
	case FormatTable:
		return Print(w, res)
	default:
		return fmt.Errorf("unknown output format %q", format)
	}
}

func writeJSON(w io.Writer, res *Result) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	if res.Columns == nil {
		out := map[string]any{"duration_ms": res.DurationMS}
		if res.RowsAffected != nil {
			out["rows_affected"] = *res.RowsAffected
		}

		return enc.Encode(out)
	}

	keys := uniqueKeys(res.Columns)
	rows := make([]map[string]any, 0, len(res.Rows))

	for _, row := range res.Rows {
		obj := make(map[string]any, len(keys))
		for i, key := range keys {
			obj[key] = row[i]
		}

		rows = append(rows, obj)
	}

	return enc.Encode(rows)
}

// uniqueKeys makes column names usable as object keys. A join routinely
// yields two columns called id; an object can hold only one, so the later
// ones become id_2, id_3, … rather than silently overwriting the first.
func uniqueKeys(columns []string) []string {
	keys := make([]string, len(columns))
	seen := make(map[string]bool, len(columns))

	for i, col := range columns {
		key := col
		for n := 2; seen[key]; n++ {
			key = fmt.Sprintf("%s_%d", col, n)
		}

		seen[key] = true
		keys[i] = key
	}

	return keys
}

func writeCSV(w io.Writer, res *Result) error {
	if res.Columns == nil {
		return nil
	}

	cw := csv.NewWriter(w)

	if err := cw.Write(res.Columns); err != nil {
		return fmt.Errorf("writing csv header, err: %w", err)
	}

	for _, row := range res.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			if v != nil {
				cells[i] = cast.ToString(v)
			}
		}

		if err := cw.Write(cells); err != nil {
			return fmt.Errorf("writing csv row, err: %w", err)
		}
	}

	cw.Flush()

	return cw.Error()
}
