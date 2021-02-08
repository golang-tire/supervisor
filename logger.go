package supervisor

import (
	"os"
	"time"

	"github.com/blendle/zapdriver"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// WithLogger is an option that allows you to provide your own customized logger.
func WithLogger(logger *zap.Logger) Option {
	return func(s *Supervisor) error {
		return assignLogger(s, logger)
	}
}

// WithDevelopmentLogger is an option that uses a zap Logger with development configurations
func WithDevelopmentLogger() Option {
	return func(s *Supervisor) error {
		logger := s.newLogger(
			zapcore.DebugLevel,
			zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
		)
		logger = logger.With(zap.String("supervisor", s.Name), zap.String("version", s.Version))
		return assignLogger(s, logger)
	}
}

// WithProductionLogger is an option that uses a zap Logger with production configurations
func WithProductionLogger() Option {
	return func(s *Supervisor) error {
		logger := s.newLogger(
			zapcore.InfoLevel,
			zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()),
		)
		logger = logger.With(zap.String("supervisor", s.Name), zap.String("version", s.Version))
		return assignLogger(s, logger)
	}
}

// WithConsoleLogger is an option that uses a zap Logger with to be used for debugging in the console
func WithConsoleLogger(level zapcore.Level) Option {
	return func(s *Supervisor) error {
		config := zap.NewProductionEncoderConfig()
		config.EncodeTime = zapcore.RFC3339TimeEncoder

		logger := s.newLogger(
			level,
			zapcore.NewConsoleEncoder(config),
		)
		return assignLogger(s, logger)
	}
}

// WithStackedLogger is an option that uses a zap production Logger compliant with the GCP/Stackdriver format.
func WithStackedLogger(level zapcore.Level) Option {
	return func(s *Supervisor) error {
		logger := s.newLogger(
			level,
			zapcore.NewJSONEncoder(zapdriver.NewProductionEncoderConfig()),
		)
		logger = logger.With(zapdriver.ServiceContext(s.Name), zapdriver.Label("version", s.Version))
		return assignLogger(s, logger)
	}
}

func assignLogger(s *Supervisor, logger *zap.Logger) error {
	undo, err := zap.RedirectStdLogAt(logger, zapcore.ErrorLevel)
	if err != nil {
		return err
	}
	s.logger = logger
	s.loggerRedirectUndo = undo
	return nil
}

func (s *Supervisor) newLogger(level zapcore.Level, encoder zapcore.Encoder) *zap.Logger {
	atom := zap.NewAtomicLevel()
	atom.SetLevel(level)

	logger := zap.New(zapcore.NewSamplerWithOptions(zapcore.NewCore(
		encoder,
		zapcore.Lock(os.Stdout),
		atom,
	), time.Second, 100, 10))

	return logger
}
