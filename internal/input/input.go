package input

import (
	"bufio"
	"context"
	"fmt"
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
	defer cReader.Close()

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
			return fmt.Errorf("reading input, err: %w", err)
		}

		if o.NoDelimeter {
			line = line[:len(line)-1]
		}

		if err := fn(ctx, string(line)); err != nil {
			slog.Error("input: error", "error", err.Error())
		}
	}
}
