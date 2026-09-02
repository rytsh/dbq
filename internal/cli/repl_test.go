package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, stdin string, args ...string) (string, error) {
	t.Helper()

	root := NewRootCommand(BuildInfo{})

	var out bytes.Buffer

	root.SetOut(&out)
	root.SetErr(&out)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append([]string{"--source", ":memory:", "--type", "sqlite3"}, args...))

	err := root.ExecuteContext(t.Context())

	return out.String(), err
}

func TestExecuteFlagJSON(t *testing.T) {
	out, err := run(t, "", "-e", "select 1 as a; select 'x' as b", "-o", "json")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	if want := "[\n  {\n    \"a\": 1\n  }\n]\n[\n  {\n    \"b\": \"x\"\n  }\n]\n"; out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
}

func TestFileFlagCSV(t *testing.T) {
	path := filepath.Join(t.TempDir(), "q.sql")
	if err := os.WriteFile(path, []byte("select 1 as a, 'x' as b;"), 0o600); err != nil {
		t.Fatal(err)
	}

	out, err := run(t, "", "-f", path, "--output", "csv")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	if want := "a,b\n1,x\n"; out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
}

func TestStdinScriptFailsFast(t *testing.T) {
	out, err := run(t, "select 1 as a; select * from nope; select 2 as b;", "-o", "csv")
	if err == nil {
		t.Fatalf("expected an error, got output %q", out)
	}

	if strings.Contains(out, "b\n2") {
		t.Errorf("statement after the failure still ran:\n%s", out)
	}
}

func TestNoDelimiterKeepsUnterminatedStatement(t *testing.T) {
	out, err := run(t, "", "-n", "-e", "select 1 as a; select 2 as b", "-o", "csv")
	if err != nil {
		t.Fatalf("run: %v\n%s", err, out)
	}

	if want := "a\n1\nb\n2\n"; out != want {
		t.Errorf("out = %q, want %q", out, want)
	}
}

func TestExecuteAndFileAreExclusive(t *testing.T) {
	if _, err := run(t, "", "-e", "select 1", "-f", "x.sql"); err == nil {
		t.Error("expected an error when both -e and -f are given")
	}
}

func TestUnknownOutputFormat(t *testing.T) {
	if _, err := run(t, "", "-e", "select 1", "-o", "xml"); err == nil {
		t.Error("expected an error for an unknown format")
	}
}
