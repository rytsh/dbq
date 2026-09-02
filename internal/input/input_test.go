package input

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func collect(t *testing.T, script string, opts ...Option) ([]string, error) {
	t.Helper()

	var got []string

	err := Input(context.Background(), strings.NewReader(script), func(_ context.Context, stmt string) error {
		got = append(got, stmt)

		return nil
	}, opts...)

	return got, err
}

func TestInputSplitsOnTopLevelSemicolons(t *testing.T) {
	got, err := collect(t, "select ';' as a;\nselect\n 2;\nselect 3")
	if err != nil {
		t.Fatalf("input: %v", err)
	}

	want := []string{"select ';' as a;", "select\n 2;", "select 3"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("statements = %q, want %q", got, want)
	}
}

func TestInputStripsDelimiter(t *testing.T) {
	got, err := collect(t, "select 1;", NoDelimeter(true))
	if err != nil {
		t.Fatalf("input: %v", err)
	}

	if len(got) != 1 || got[0] != "select 1" {
		t.Errorf("statements = %q, want [select 1]", got)
	}
}

// TestInputStopsOnErrorWhenNotInteractive: a script must fail fast, whereas a
// person at a terminal gets the error and a fresh prompt.
func TestInputStopsOnErrorWhenNotInteractive(t *testing.T) {
	boom := errors.New("boom")
	calls := 0

	err := Input(context.Background(), strings.NewReader("select 1; select 2;"), func(context.Context, string) error {
		calls++

		return boom
	})

	if !errors.Is(err, boom) || calls != 1 {
		t.Errorf("err = %v calls = %d, want boom after one call", err, calls)
	}

	calls = 0

	err = Input(context.Background(), strings.NewReader("select 1; select 2;"), func(context.Context, string) error {
		calls++

		return boom
	}, Interactive(true))

	if err != nil || calls != 2 {
		t.Errorf("interactive: err = %v calls = %d, want nil after two calls", err, calls)
	}
}

func TestInputIgnoresBlankInput(t *testing.T) {
	got, err := collect(t, "\n ; \n\n")
	if err != nil {
		t.Fatalf("input: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("statements = %q, want none", got)
	}
}
