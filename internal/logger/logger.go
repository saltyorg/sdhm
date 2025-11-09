package logger

import (
	"log"
)

// Logger provides structured logging with levels
type Logger struct {
	stdLogger *log.Logger
}

// New creates a new Logger wrapping a standard log.Logger
func New(stdLogger *log.Logger) *Logger {
	return &Logger{
		stdLogger: stdLogger,
	}
}

// Info logs an informational message
func (l *Logger) Info(format string, v ...any) {
	l.stdLogger.Printf("INFO: "+format, v...)
}

// Warn logs a warning message
func (l *Logger) Warn(format string, v ...any) {
	l.stdLogger.Printf("WARN: "+format, v...)
}

// Error logs an error message
func (l *Logger) Error(format string, v ...any) {
	l.stdLogger.Printf("ERROR: "+format, v...)
}

// Printf provides compatibility with existing code that uses Printf directly
// It's a pass-through to the underlying logger
func (l *Logger) Printf(format string, v ...any) {
	l.stdLogger.Printf(format, v...)
}

// Println provides compatibility with existing code that uses Println directly
// It's a pass-through to the underlying logger
func (l *Logger) Println(v ...any) {
	l.stdLogger.Println(v...)
}

// GetStdLogger returns the underlying standard logger for compatibility
// This is useful when passing to functions that expect *log.Logger
func (l *Logger) GetStdLogger() *log.Logger {
	return l.stdLogger
}

// LogFunc returns a logging function compatible with func(string, ...any)
// This is useful for passing to functions that need a simple log function
func (l *Logger) LogFunc() func(string, ...any) {
	return func(format string, v ...any) {
		l.stdLogger.Printf(format, v...)
	}
}
