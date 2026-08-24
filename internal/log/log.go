package log

import (
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

func NewLogger(level zapcore.Level) (*zap.Logger, error) {
	config := zap.NewProductionConfig()
	config.Level = zap.NewAtomicLevelAt(level)
	config.EncoderConfig.CallerKey = zapcore.OmitKey
	config.EncoderConfig.EncodeTime = zapcore.RFC3339NanoTimeEncoder
	config.DisableStacktrace = true
	return config.Build()
}
