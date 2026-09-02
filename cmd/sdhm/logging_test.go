package main

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

func TestProcessLoggerMapsNativeJournalPriorities(t *testing.T) {
	tests := []struct {
		name       string
		log        func(*slog.Logger)
		wantPrefix string
	}{
		{"debug", func(l *slog.Logger) { l.Debug("debug") }, "<7>"},
		{"info", func(l *slog.Logger) { l.Info("info") }, "<6>"},
		{"warn", func(l *slog.Logger) { l.Warn("warn") }, "<4>"},
		{"error", func(l *slog.Logger) { l.Error("error") }, "<3>"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := newProcessLoggerWithOptions(&output, true, &slog.HandlerOptions{Level: slog.LevelDebug})
			tt.log(logger.With("component", "daemon"))

			got := output.String()
			if !strings.HasPrefix(got, tt.wantPrefix) || !strings.Contains(got, "component=daemon") {
				t.Fatalf("journal output = %q, want prefix %q and retained attrs", got, tt.wantPrefix)
			}
		})
	}
}

func TestProcessLoggerWithoutJournalMatchesTextHandler(t *testing.T) {
	removeTime := &slog.HandlerOptions{
		Level: slog.LevelDebug,
		ReplaceAttr: func(groups []string, attr slog.Attr) slog.Attr {
			if attr.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return attr
		},
	}

	var got bytes.Buffer
	newProcessLoggerWithOptions(&got, false, removeTime).With("component", "daemon").Warn("warning")

	var want bytes.Buffer
	slog.New(slog.NewTextHandler(&want, removeTime)).With("component", "daemon").Warn("warning")

	if got.String() != want.String() {
		t.Fatalf("non-journal output = %q, want standard text output %q", got.String(), want.String())
	}
	if strings.HasPrefix(got.String(), "<") {
		t.Fatalf("non-journal output = %q, want no journal priority prefix", got.String())
	}
}

func TestProcessLoggerRetainsAttributesAndGroups(t *testing.T) {
	var output bytes.Buffer
	logger := newProcessLoggerWithOptions(&output, true, &slog.HandlerOptions{Level: slog.LevelDebug})
	logger.With("component", "daemon").WithGroup("request").Info("started", "id", "123")

	got := output.String()
	if !strings.HasPrefix(got, "<6>") || !strings.Contains(got, "component=daemon") || !strings.Contains(got, "request.id=123") {
		t.Fatalf("journal output = %q, want info prefix and retained attrs/groups", got)
	}
}

func TestProcessLoggerWritesConcurrentRecordsAsCompleteLines(t *testing.T) {
	const records = 100

	var output bytes.Buffer
	logger := newProcessLoggerWithOptions(&output, true, &slog.HandlerOptions{Level: slog.LevelDebug})
	levels := []struct {
		name   string
		prefix string
		log    func(*slog.Logger, string)
	}{
		{"debug", "<7>", func(l *slog.Logger, message string) { l.Debug(message) }},
		{"info", "<6>", func(l *slog.Logger, message string) { l.Info(message) }},
		{"warn", "<4>", func(l *slog.Logger, message string) { l.Warn(message) }},
		{"error", "<3>", func(l *slog.Logger, message string) { l.Error(message) }},
	}

	var wg sync.WaitGroup
	for i := range records {
		level := levels[i%len(levels)]
		message := fmt.Sprintf("%s-%d", level.name, i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			level.log(logger, message)
		}()
	}
	wg.Wait()

	lines := strings.Split(strings.TrimSuffix(output.String(), "\n"), "\n")
	if len(lines) != records {
		t.Fatalf("got %d lines, want %d: %q", len(lines), records, output.String())
	}
	for _, line := range lines {
		matched := false
		for _, level := range levels {
			if strings.HasPrefix(line, level.prefix) && strings.Contains(line, "msg="+level.name+"-") {
				matched = true
				break
			}
		}
		if !matched {
			t.Fatalf("line = %q, want one complete line with its journal priority", line)
		}
	}
}

func TestWriteProcessError(t *testing.T) {
	tests := []struct {
		name    string
		journal bool
		err     error
		want    string
	}{
		{
			name:    "journal joined error",
			journal: true,
			err:     errors.Join(errors.New("primary"), errors.New("cleanup")),
			want:    "<3>primary; cleanup\n",
		},
		{
			name:    "journal mixed newlines",
			journal: true,
			err:     errors.New("first\r\nsecond\rthird\nfourth"),
			want:    "<3>first; second; third; fourth\n",
		},
		{
			name:    "interactive joined error",
			journal: false,
			err:     errors.Join(errors.New("primary"), errors.New("cleanup")),
			want:    "primary\ncleanup\n",
		},
		{
			name:    "interactive mixed newlines",
			journal: false,
			err:     errors.New("first\r\nsecond\rthird\nfourth"),
			want:    "first\r\nsecond\rthird\nfourth\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var output bytes.Buffer
			writeProcessError(&output, tt.journal, tt.err)
			if got := output.String(); got != tt.want {
				t.Errorf("writeProcessError() = %q, want %q", got, tt.want)
			}
			if tt.journal {
				got := output.String()
				if prefixes := strings.Count(got, "<3>"); prefixes != 1 {
					t.Errorf("journal priority prefixes = %d, want 1 in %q", prefixes, got)
				}
				if terminators := strings.Count(got, "\n"); terminators != 1 {
					t.Errorf("journal record terminators = %d, want 1 in %q", terminators, got)
				}
				if strings.ContainsRune(got, '\r') {
					t.Errorf("journal output contains a carriage return: %q", got)
				}
			}
		})
	}
}

func TestJournalStreamPresent(t *testing.T) {
	tests := []struct {
		name  string
		value string
		want  bool
	}{
		{"absent", "", false},
		{"present", "8:12345", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("JOURNAL_STREAM", tt.value)
			if got := journalStreamPresent(); got != tt.want {
				t.Fatalf("journalStreamPresent() = %t, want %t", got, tt.want)
			}
		})
	}
}
