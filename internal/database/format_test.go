package database

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func sampleResult() *Result {
	return &Result{
		Columns:  []string{"id", "name"},
		Rows:     [][]any{{int64(1), "ada"}, {int64(2), nil}},
		RowCount: 2,
		Kind:     KindRead,
		Duration: 3 * time.Millisecond,
	}
}

func TestParseFormat(t *testing.T) {
	for _, s := range []string{"table", "json", "csv", "JSON"} {
		if _, err := ParseFormat(s); err != nil {
			t.Errorf("ParseFormat(%q): %v", s, err)
		}
	}

	if _, err := ParseFormat("xml"); err == nil {
		t.Error("ParseFormat(xml) accepted")
	}
}

func TestWriteJSON(t *testing.T) {
	var out bytes.Buffer

	if err := Write(&out, sampleResult(), FormatJSON); err != nil {
		t.Fatalf("write: %v", err)
	}

	want := "[\n  {\n    \"id\": 1,\n    \"name\": \"ada\"\n  },\n  {\n    \"id\": 2,\n    \"name\": null\n  }\n]\n"
	if out.String() != want {
		t.Errorf("json = %q, want %q", out.String(), want)
	}
}

// TestWriteJSONDuplicateColumns: a join often yields two `id` columns; an
// object cannot hold both under one key, so the second is renamed rather than
// silently dropped.
func TestWriteJSONDuplicateColumns(t *testing.T) {
	var out bytes.Buffer

	res := &Result{Columns: []string{"id", "id", "id_2"}, Rows: [][]any{{1, 2, 3}}, RowCount: 1}
	if err := Write(&out, res, FormatJSON); err != nil {
		t.Fatalf("write: %v", err)
	}

	want := "[\n  {\n    \"id\": 1,\n    \"id_2\": 2,\n    \"id_2_2\": 3\n  }\n]\n"
	if out.String() != want {
		t.Errorf("json = %q, want %q", out.String(), want)
	}
}

func TestWriteJSONExec(t *testing.T) {
	var out bytes.Buffer

	affected := int64(4)
	if err := Write(&out, &Result{Kind: KindWrite, RowsAffected: &affected}, FormatJSON); err != nil {
		t.Fatalf("write: %v", err)
	}

	if !strings.Contains(out.String(), `"rows_affected": 4`) {
		t.Errorf("json = %q, want rows_affected", out.String())
	}
}

func TestWriteCSV(t *testing.T) {
	var out bytes.Buffer

	if err := Write(&out, sampleResult(), FormatCSV); err != nil {
		t.Fatalf("write: %v", err)
	}

	if want := "id,name\n1,ada\n2,\n"; out.String() != want {
		t.Errorf("csv = %q, want %q", out.String(), want)
	}
}

func TestWriteTable(t *testing.T) {
	var out bytes.Buffer

	if err := Write(&out, sampleResult(), FormatTable); err != nil {
		t.Fatalf("write: %v", err)
	}

	if !strings.Contains(out.String(), "| id | name |") || !strings.Contains(out.String(), "|  1 | ada  |") {
		t.Errorf("table = %q", out.String())
	}
}
