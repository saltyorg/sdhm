package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
)

type priorityTextHandler struct {
	debug slog.Handler
	info  slog.Handler
	warn  slog.Handler
	err   slog.Handler
}

type priorityWriter struct {
	out    io.Writer
	mu     *sync.Mutex
	prefix string
}

func (w *priorityWriter) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if _, err := io.WriteString(w.out, w.prefix); err != nil {
		return 0, err
	}
	return w.out.Write(data)
}

func (h *priorityTextHandler) handler(level slog.Level) slog.Handler {
	switch {
	case level >= slog.LevelError:
		return h.err
	case level >= slog.LevelWarn:
		return h.warn
	case level >= slog.LevelInfo:
		return h.info
	default:
		return h.debug
	}
}

func (h *priorityTextHandler) Enabled(ctx context.Context, level slog.Level) bool {
	return h.handler(level).Enabled(ctx, level)
}

func (h *priorityTextHandler) Handle(ctx context.Context, record slog.Record) error {
	return h.handler(record.Level).Handle(ctx, record)
}

func (h *priorityTextHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return &priorityTextHandler{
		debug: h.debug.WithAttrs(attrs),
		info:  h.info.WithAttrs(attrs),
		warn:  h.warn.WithAttrs(attrs),
		err:   h.err.WithAttrs(attrs),
	}
}

func (h *priorityTextHandler) WithGroup(name string) slog.Handler {
	return &priorityTextHandler{
		debug: h.debug.WithGroup(name),
		info:  h.info.WithGroup(name),
		warn:  h.warn.WithGroup(name),
		err:   h.err.WithGroup(name),
	}
}

func newProcessLogger(out io.Writer, journal bool) *slog.Logger {
	return newProcessLoggerWithOptions(out, journal, nil)
}

func newProcessLoggerWithOptions(out io.Writer, journal bool, options *slog.HandlerOptions) *slog.Logger {
	if !journal {
		return slog.New(slog.NewTextHandler(out, options))
	}

	mu := new(sync.Mutex)
	return slog.New(&priorityTextHandler{
		debug: slog.NewTextHandler(&priorityWriter{out: out, mu: mu, prefix: "<7>"}, options),
		info:  slog.NewTextHandler(&priorityWriter{out: out, mu: mu, prefix: "<6>"}, options),
		warn:  slog.NewTextHandler(&priorityWriter{out: out, mu: mu, prefix: "<4>"}, options),
		err:   slog.NewTextHandler(&priorityWriter{out: out, mu: mu, prefix: "<3>"}, options),
	})
}

func journalStreamPresent() bool {
	return os.Getenv("JOURNAL_STREAM") != ""
}

func writeProcessError(out io.Writer, journal bool, err error) {
	if journal {
		_, _ = fmt.Fprintf(out, "<3>%v\n", err)
		return
	}
	_, _ = fmt.Fprintln(out, err)
}
