// Package input reads SQL statements from a stream and hands them to a runner
// one at a time.
package input

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	"github.com/muesli/cancelreader"

	"github.com/rytsh/dbq/internal/database"
)

type option struct {
	NoDelimeter bool
	// Interactive is nil until set, so the terminal can be auto-detected.
	Interactive *bool
}

func (o *option) Apply(opts ...Option) {
	for _, opt := range opts {
		opt(o)
	}
}

// Option configures Input.
type Option func(*option)

// NoDelimeter strips the trailing `;` before a statement is run, for drivers
// such as Oracle that reject it.
func NoDelimeter(v bool) Option {
	return func(o *option) {
		o.NoDelimeter = v
	}
}

// Interactive forces terminal behaviour on or off: a prompt is shown and an
// error does not end the session. When unset, it is on exactly when the reader
// is a terminal.
func Interactive(v bool) Option {
	return func(o *option) {
		o.Interactive = &v
	}
}

// Input reads statements from r until EOF or ctx is cancelled.
//
// Statements are split at top-level semicolons using the SQL lexer, so a
// semicolon inside a string literal or a comment does not end a statement, and
// a statement may span lines. In interactive mode a failing statement is
// reported and the loop continues; otherwise the first error ends the run so a
// script fails fast.
func Input(ctx context.Context, r io.Reader, fn func(ctx context.Context, input string) error, opts ...Option) error {
	o := &option{}
	o.Apply(opts...)

	interactive := isTerminal(r)
	if o.Interactive != nil {
		interactive = *o.Interactive
	}

	cReader, closeReader := cancellable(ctx, r)
	defer closeReader()

	s := &session{
		ctx:         ctx,
		reader:      bufio.NewReader(cReader),
		fn:          fn,
		noDelimiter: o.NoDelimeter,
		interactive: interactive,
	}

	return s.loop()
}

// session is one run of the statement loop.
type session struct {
	ctx         context.Context
	reader      *bufio.Reader
	fn          func(ctx context.Context, input string) error
	noDelimiter bool
	interactive bool
	buffer      strings.Builder
}

func (s *session) loop() error {
	for {
		select {
		case <-s.ctx.Done():
			return nil
		default:
		}

		if s.interactive {
			prompt(strings.TrimSpace(s.buffer.String()) != "")
		}

		line, err := s.reader.ReadString('\n')
		s.buffer.WriteString(line)

		// Re-lexing the whole buffer on every line is only worth it when
		// the new line could have completed a statement.
		if strings.Contains(line, ";") {
			if err := s.runComplete(); err != nil {
				return err
			}
		}

		if err == nil {
			continue
		}

		return s.finish(err)
	}
}

// runComplete executes every terminated statement in the buffer and keeps
// the unterminated remainder.
func (s *session) runComplete() error {
	complete, rest := database.SplitComplete(s.buffer.String())

	for _, stmt := range complete {
		if err := s.run(stmt); err != nil {
			return err
		}
	}

	s.buffer.Reset()
	s.buffer.WriteString(rest)

	return nil
}

// finish handles the end of input. EOF is how a piped script or a Ctrl-D ends
// the session, and cancelreader reports a cancelled read as ErrCanceled;
// neither is a failure.
func (s *session) finish(err error) error {
	if !errors.Is(err, io.EOF) && !errors.Is(err, cancelreader.ErrCanceled) {
		return fmt.Errorf("reading input, err: %w", err)
	}

	if s.interactive {
		fmt.Println()
	}

	// A trailing statement without the delimiter still has to run.
	if rest := strings.TrimSpace(s.buffer.String()); rest != "" && !errors.Is(err, cancelreader.ErrCanceled) {
		return s.run(rest)
	}

	return nil
}

// run hands one statement to the callback. In interactive mode a failure is
// reported and the session goes on; otherwise it ends the run.
func (s *session) run(stmt string) error {
	if s.noDelimiter {
		stmt = strings.TrimSuffix(stmt, ";")
	}

	err := s.fn(s.ctx, stmt)
	if err == nil || !s.interactive {
		return err
	}

	slog.Error("input: error", "error", err.Error())

	return nil
}

// cancellable wraps r so a blocked read returns when ctx is cancelled, which
// is what lets Ctrl-C interrupt a waiting prompt. A regular file cannot be
// polled and cancelreader refuses it; such input is finite, so it is read
// plainly and the cancellation is honoured between statements instead.
func cancellable(ctx context.Context, r io.Reader) (io.Reader, func()) {
	cReader, err := cancelreader.NewReader(r)
	if err != nil {
		return r, func() {}
	}

	done := make(chan struct{})

	go func() {
		select {
		case <-ctx.Done():
			cReader.Cancel()
		case <-done:
		}
	}()

	return cReader, func() {
		close(done)
		_ = cReader.Close()
	}
}

func prompt(continuation bool) {
	if continuation {
		fmt.Print("  > ")

		return
	}

	fmt.Print("> ")
}

// isTerminal reports whether r is a character device, i.e. a person typing
// rather than a pipe or file.
func isTerminal(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}

	info, err := f.Stat()
	if err != nil {
		return false
	}

	return info.Mode()&os.ModeCharDevice != 0
}
