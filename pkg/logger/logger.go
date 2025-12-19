// Package logger provides a structured logging solution using Zap.
// It supports both development (console-friendly) and production (JSON) modes.
package logger

import (
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// New creates a new Zap logger instance.
// If devMode is true, it returns a development logger with console-friendly output.
// If devMode is false, it returns a production logger with JSON structured output.
func New(devMode bool) (*zap.Logger, error) {
	var config zap.Config

	if devMode {
		// Development configuration: human-readable, colored console output
		config = zap.NewDevelopmentConfig()
		config.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
		config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	} else {
		// Production configuration: JSON structured logs for machine parsing
		config = zap.NewProductionConfig()
		config.EncoderConfig.TimeKey = "timestamp"
		config.EncoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
	}

	// Build and return the logger
	logger, err := config.Build(
		zap.AddCallerSkip(0), // Preserve accurate caller information
	)
	if err != nil {
		return nil, err
	}

	return logger, nil
}

// NewWithWriter creates a logger that writes to a custom writer (useful for testing).
func NewWithWriter(devMode bool, writer zapcore.WriteSyncer) *zap.Logger {
	var encoder zapcore.Encoder
	var level zapcore.Level

	if devMode {
		encoderConfig := zap.NewDevelopmentEncoderConfig()
		encoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
		encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
		encoder = zapcore.NewConsoleEncoder(encoderConfig)
		level = zapcore.DebugLevel
	} else {
		encoderConfig := zap.NewProductionEncoderConfig()
		encoderConfig.TimeKey = "timestamp"
		encoderConfig.EncodeTime = zapcore.ISO8601TimeEncoder
		encoder = zapcore.NewJSONEncoder(encoderConfig)
		level = zapcore.InfoLevel
	}

	core := zapcore.NewCore(encoder, writer, level)
	return zap.New(core, zap.AddCaller())
}

// Sync flushes any buffered log entries.
// Applications should take care to call Sync before exiting.
func Sync(logger *zap.Logger) {
	// Ignore sync errors on stdout/stderr as they're expected in some environments
	_ = logger.Sync()
}

// Fatal logs a fatal message and exits the application.
// This is a convenience wrapper that ensures log sync before exit.
func Fatal(logger *zap.Logger, msg string, fields ...zap.Field) {
	logger.Fatal(msg, fields...)
	os.Exit(1) // This line is unreachable but makes intent clear
}
