package logger

import (
	"bytes"
	"log"
	"strings"
	"testing"
)

func TestLogger_Info(t *testing.T) {
	var buf bytes.Buffer
	stdLogger := log.New(&buf, "", 0)
	logger := New(stdLogger)

	logger.Info("test message")
	output := buf.String()

	if !strings.Contains(output, "INFO: test message") {
		t.Errorf("Expected output to contain 'INFO: test message', got: %s", output)
	}
}

func TestLogger_Warn(t *testing.T) {
	var buf bytes.Buffer
	stdLogger := log.New(&buf, "", 0)
	logger := New(stdLogger)

	logger.Warn("test warning")
	output := buf.String()

	if !strings.Contains(output, "WARN: test warning") {
		t.Errorf("Expected output to contain 'WARN: test warning', got: %s", output)
	}
}

func TestLogger_Error(t *testing.T) {
	var buf bytes.Buffer
	stdLogger := log.New(&buf, "", 0)
	logger := New(stdLogger)

	logger.Error("test error")
	output := buf.String()

	if !strings.Contains(output, "ERROR: test error") {
		t.Errorf("Expected output to contain 'ERROR: test error', got: %s", output)
	}
}

func TestLogger_WithFormatting(t *testing.T) {
	var buf bytes.Buffer
	stdLogger := log.New(&buf, "", 0)
	logger := New(stdLogger)

	logger.Info("container %s on network %s", "web", "bridge")
	output := buf.String()

	if !strings.Contains(output, "INFO: container web on network bridge") {
		t.Errorf("Expected formatted output, got: %s", output)
	}
}

func TestLogger_Printf_Compatibility(t *testing.T) {
	var buf bytes.Buffer
	stdLogger := log.New(&buf, "", 0)
	logger := New(stdLogger)

	logger.Printf("INFO: legacy format")
	output := buf.String()

	if !strings.Contains(output, "INFO: legacy format") {
		t.Errorf("Printf should pass through to underlying logger, got: %s", output)
	}
}

func TestLogger_GetStdLogger(t *testing.T) {
	stdLogger := log.New(&bytes.Buffer{}, "", 0)
	logger := New(stdLogger)

	if logger.GetStdLogger() != stdLogger {
		t.Error("GetStdLogger should return the underlying logger")
	}
}

func TestLogger_LogFunc(t *testing.T) {
	var buf bytes.Buffer
	stdLogger := log.New(&buf, "", 0)
	logger := New(stdLogger)

	logFunc := logger.LogFunc()
	logFunc("INFO: test %s", "message")

	output := buf.String()
	if !strings.Contains(output, "INFO: test message") {
		t.Errorf("LogFunc should work correctly, got: %s", output)
	}
}
