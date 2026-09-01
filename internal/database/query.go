package database

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"time"
	"unicode/utf8"

	"github.com/jmoiron/sqlx"
	"github.com/olekukonko/tablewriter"
	"github.com/spf13/cast"
)

// DefaultMaxRows caps the rows materialized for a single query when the caller
// does not ask for a specific limit. Results are buffered in memory, so an
// unbounded SELECT on a large table would otherwise blow up the process.
const DefaultMaxRows = 500

// Result is a driver-independent, JSON-serializable query result.
type Result struct {
	// Columns are the result set column names, in order. Nil for Exec results.
	Columns []string `json:"columns,omitempty"`
	// Rows holds the result set. Values are normalized to JSON-friendly types.
	Rows [][]any `json:"rows,omitempty"`
	// RowCount is len(Rows).
	RowCount int `json:"row_count"`
	// Truncated is true when the result set hit the row limit and more rows exist.
	Truncated bool `json:"truncated,omitempty"`
	// CellsTruncated counts values that were shortened to the cell limit.
	// Non-zero means the displayed values are prefixes, not the stored data.
	CellsTruncated int `json:"cells_truncated,omitempty"`
	// RowsAffected is set for statements executed via Exec. Nil for queries.
	RowsAffected *int64 `json:"rows_affected,omitempty"`
	// Kind is how dbq classified the statement.
	Kind Kind `json:"kind"`
	// Duration is the wall-clock execution time.
	Duration time.Duration `json:"-"`
	// DurationMS is Duration in milliseconds, for JSON consumers.
	DurationMS int64 `json:"duration_ms"`
}

// DefaultMaxCellChars caps the characters returned for a single text value.
//
// A row cap alone does not bound the response: one TEXT or JSON column can hold
// megabytes, so `SELECT * FROM documents` can blow a model's context window in
// a single call even at a small row limit.
const DefaultMaxCellChars = 500

// QueryOptions tunes a single statement execution.
type QueryOptions struct {
	// MaxRows limits the rows read from the result set. Zero means DefaultMaxRows.
	// A negative value disables the limit.
	MaxRows int
	// MaxCellChars limits the characters kept per text value. Zero means
	// DefaultMaxCellChars. A negative value disables the limit.
	MaxCellChars int
	// Timeout bounds the statement. Zero means no dbq-imposed deadline.
	Timeout time.Duration
}

func (o QueryOptions) maxRows() int {
	switch {
	case o.MaxRows == 0:
		return DefaultMaxRows
	case o.MaxRows < 0:
		return -1
	default:
		return o.MaxRows
	}
}

func (o QueryOptions) maxCellChars() int {
	switch {
	case o.MaxCellChars == 0:
		return DefaultMaxCellChars
	case o.MaxCellChars < 0:
		return -1
	default:
		return o.MaxCellChars
	}
}

// Execute runs a single statement and returns a Result.
//
// Statements classified as read return a row set; everything else goes through
// Exec so that rows-affected is reported and no result set is expected. The
// caller is responsible for authorizing the statement first (see Authorize).
func Execute(ctx context.Context, db *sqlx.DB, sql string, opts QueryOptions) (*Result, error) {
	kind := Classify(sql)

	// A deadline is the only thing standing between an agent-issued cartesian
	// join and a connection pinned until the client gives up.
	if opts.Timeout > 0 {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}

	start := time.Now()

	if !kind.ReturnsRows() {
		res, err := db.ExecContext(ctx, sql)
		if err != nil {
			return nil, fmt.Errorf("executing statement, err: %w", err)
		}

		out := &Result{Kind: kind}
		out.setDuration(time.Since(start))

		// Not every driver implements RowsAffected; absence is not an error.
		if affected, err := res.RowsAffected(); err == nil {
			out.RowsAffected = &affected
		}

		return out, nil
	}

	rows, err := db.QueryxContext(ctx, sql)
	if err != nil {
		return nil, fmt.Errorf("querying database, err: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out, err := scanRows(rows, opts.maxRows(), opts.maxCellChars())
	if err != nil {
		return nil, err
	}

	out.Kind = kind
	out.setDuration(time.Since(start))

	return out, nil
}

func (r *Result) setDuration(d time.Duration) {
	r.Duration = d
	r.DurationMS = d.Milliseconds()
}

func scanRows(rows *sqlx.Rows, limit, cellLimit int) (*Result, error) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, fmt.Errorf("reading columns, err: %w", err)
	}

	out := &Result{Columns: columns, Rows: [][]any{}}

	for rows.Next() {
		if limit >= 0 && len(out.Rows) >= limit {
			// One more row exists beyond the limit.
			out.Truncated = true

			break
		}

		values, err := rows.SliceScan()
		if err != nil {
			return nil, fmt.Errorf("scanning row, err: %w", err)
		}

		normalized := make([]any, len(values))

		for i, v := range values {
			value := normalize(v)

			if text, ok := value.(string); ok {
				truncated, cut := truncateCell(text, cellLimit)
				if cut {
					out.CellsTruncated++
				}

				value = truncated
			}

			normalized[i] = value
		}

		out.Rows = append(out.Rows, normalized)
	}

	// rows.Err reports errors that ended the iteration early, which would
	// otherwise be indistinguishable from a short result set.
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating rows, err: %w", err)
	}

	out.RowCount = len(out.Rows)

	return out, nil
}

// truncateCell shortens an oversized text value, marking it so the reader knows
// the value is a prefix rather than the stored data.
func truncateCell(s string, limit int) (string, bool) {
	if limit < 0 || len(s) <= limit {
		return s, false
	}

	runes := []rune(s)
	if len(runes) <= limit {
		return s, false
	}

	return string(runes[:limit]) + fmt.Sprintf("… [truncated, %d chars total]", len(runes)), true
}

// normalize converts driver values into types that survive a JSON round trip.
func normalize(v any) any {
	switch value := v.(type) {
	case nil:
		return nil
	case []byte:
		// Drivers return []byte for both text and binary columns. Valid UTF-8
		// is almost always text; anything else is base64 so it stays lossless.
		if utf8.Valid(value) {
			return string(value)
		}

		return base64.StdEncoding.EncodeToString(value)
	case time.Time:
		return value.Format(time.RFC3339Nano)
	case bool, string, float32, float64,
		int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64:
		return value
	default:
		return cast.ToString(value)
	}
}

// Print renders a Result as an ASCII table, the REPL output format.
func Print(w io.Writer, res *Result) error {
	if res.Columns == nil {
		if res.RowsAffected != nil {
			_, err := fmt.Fprintf(w, "OK, %d row(s) affected (%s)\n", *res.RowsAffected, res.Duration)

			return err
		}

		_, err := fmt.Fprintf(w, "OK (%s)\n", res.Duration)

		return err
	}

	table := tablewriter.NewWriter(w)
	table.SetAutoWrapText(false)
	table.SetAutoFormatHeaders(false)
	table.SetHeader(res.Columns)

	for _, row := range res.Rows {
		cells := make([]string, len(row))
		for i, v := range row {
			cells[i] = cast.ToString(v)
		}

		table.Append(cells)
	}

	table.Render()

	if res.Truncated {
		if _, err := fmt.Fprintf(w, "(truncated at %d rows)\n", res.RowCount); err != nil {
			return err
		}
	}

	if res.CellsTruncated > 0 {
		if _, err := fmt.Fprintf(w, "(%d value(s) shortened)\n", res.CellsTruncated); err != nil {
			return err
		}
	}

	return nil
}
