package input

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"

	"github.com/muesli/cancelreader"
)

type option struct {
	NoDelimeter bool
}

func (o *option) Apply(opts ...Option) {
	for _, opt := range opts {
		opt(o)
	}
}

type Option func(*option)

func NoDelimeter(v bool) Option {
	return func(o *option) {
		o.NoDelimeter = v
	}
}

func Input(ctx context.Context, fn func(ctx context.Context, input string) error, opts ...Option) error {
	o := &option{}
	o.Apply(opts...)

	// Create a new buffered reader from standard input
	cReader, err := cancelreader.NewReader(os.Stdin)
	if err != nil {
		return fmt.Errorf("creating reader, err: %w", err)
	}
	defer func() { _ = cReader.Close() }()

	go func() {
		<-ctx.Done()
		cReader.Cancel()
	}()

	reader := bufio.NewReader(cReader)

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}

		fmt.Printf("> ")

		line, err := reader.ReadBytes(';')
		if err != nil {
			// EOF is how a piped script or a Ctrl-D ends the session, and
			// cancelreader reports a cancelled read as ErrCanceled. Neither is
			// a failure.
			if errors.Is(err, io.EOF) || errors.Is(err, cancelreader.ErrCanceled) {
				fmt.Println()

				return nil
			}

			return fmt.Errorf("reading input, err: %w", err)
		}

		// A trailing statement without the delimiter still has to run.
		if len(line) == 0 {
			continue
		}

		if o.NoDelimeter && line[len(line)-1] == ';' {
			line = line[:len(line)-1]
		}

		if err := fn(ctx, string(line)); err != nil {
			slog.Error("input: error", "error", err.Error())
		}
	}
}
