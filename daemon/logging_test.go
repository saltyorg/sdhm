package daemon

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"testing"
)

type logRecorder struct {
	mu      sync.Mutex
	records []slog.Record
}

type logRecordingHandler struct {
	recorder *logRecorder
	attrs    []slog.Attr
	groups   []string
}

func newLogRecorder() *logRecorder {
	return &logRecorder{}
}

func TestLogRecorderRetainsAttributesAndGroups(t *testing.T) {
	recorder := newLogRecorder()
	slog.New(recorder).
		With("component", "daemon").
		WithGroup("recovery").
		Warn("restored", "from", "backup")

	records := recorder.Records()
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	var attrs []slog.Attr
	records[0].Attrs(func(attr slog.Attr) bool {
		attrs = append(attrs, attr)
		return true
	})
	if len(attrs) != 2 {
		t.Fatalf("record attrs = %v, want component and recovery group", attrs)
	}
	if !attrs[0].Equal(slog.String("component", "daemon")) {
		t.Errorf("first record attr = %v, want component=daemon", attrs[0])
	}
	if !attrs[1].Equal(slog.Group("recovery", slog.String("from", "backup"))) {
		t.Errorf("second record attr = %v, want recovery.from=backup", attrs[1])
	}
}

func (*logRecorder) Enabled(context.Context, slog.Level) bool {
	return true
}

func (r *logRecorder) Handle(_ context.Context, record slog.Record) error {
	r.record(record)
	return nil
}

func (r *logRecorder) record(record slog.Record) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.records = append(r.records, record.Clone())
}

func (r *logRecorder) WithAttrs(attrs []slog.Attr) slog.Handler {
	return (&logRecordingHandler{recorder: r}).WithAttrs(attrs)
}

func (r *logRecorder) WithGroup(name string) slog.Handler {
	return (&logRecordingHandler{recorder: r}).WithGroup(name)
}

func (r *logRecorder) Records() []slog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()

	records := make([]slog.Record, len(r.records))
	for i, record := range r.records {
		records[i] = record.Clone()
	}
	return records
}

func logRecords(records []slog.Record, level slog.Level, message string) []slog.Record {
	matching := make([]slog.Record, 0)
	for _, record := range records {
		if record.Level == level && record.Message == message {
			matching = append(matching, record)
		}
	}
	return matching
}

func logAttr(t *testing.T, record slog.Record, key string) slog.Value {
	t.Helper()
	var value slog.Value
	found := false
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == key {
			value = attr.Value
			found = true
			return false
		}
		return true
	})
	if !found {
		t.Fatalf("record %q is missing %q", record.Message, key)
	}
	return value
}

func logAttrIfPresent(record slog.Record, key string) (slog.Value, bool) {
	var value slog.Value
	found := false
	record.Attrs(func(attr slog.Attr) bool {
		if attr.Key == key {
			value = attr.Value
			found = true
			return false
		}
		return true
	})
	return value, found
}

func (h *logRecordingHandler) Enabled(context.Context, slog.Level) bool {
	return true
}

func (h *logRecordingHandler) Handle(_ context.Context, record slog.Record) error {
	stored := slog.NewRecord(record.Time, record.Level, record.Message, record.PC)
	stored.AddAttrs(h.attrs...)
	record.Attrs(func(attr slog.Attr) bool {
		stored.AddAttrs(h.withGroups(attr)...)
		return true
	})
	h.recorder.record(stored)
	return nil
}

func (h *logRecordingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	copy := *h
	copy.attrs = append(slices.Clone(h.attrs), h.withGroups(attrs...)...)
	copy.groups = slices.Clone(h.groups)
	return &copy
}

func (h *logRecordingHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	copy := *h
	copy.attrs = slices.Clone(h.attrs)
	copy.groups = append(slices.Clone(h.groups), name)
	return &copy
}

func (h *logRecordingHandler) withGroups(attrs ...slog.Attr) []slog.Attr {
	grouped := slices.Clone(attrs)
	for i := len(h.groups) - 1; i >= 0; i-- {
		grouped = []slog.Attr{{
			Key:   h.groups[i],
			Value: slog.GroupValue(grouped...),
		}}
	}
	return grouped
}
