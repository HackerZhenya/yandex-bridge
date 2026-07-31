// Package logging configures the process-wide structured logger and routes the
// HAP library's own output through it, so a pairing problem and a Yandex API
// problem land in the same stream.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"

	haplog "github.com/brutella/hap/log"
)

// Setup builds a JSON logger at the given level and installs it as the default.
func Setup(level string, w io.Writer) (*slog.Logger, error) {
	lvl, err := ParseLevel(level)
	if err != nil {
		return nil, err
	}
	logger := slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(logger)
	BridgeHAP(logger, lvl <= slog.LevelDebug)
	return logger, nil
}

// SetupStderr is Setup writing to stderr.
func SetupStderr(level string) (*slog.Logger, error) {
	return Setup(level, os.Stderr)
}

// ParseLevel maps a config string to a slog level.
func ParseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("unknown log level %q (want debug, info, warn or error)", s)
	}
}

// BridgeHAP redirects github.com/brutella/hap/log into slog. HAP's own loggers
// are plain *log.Logger values writing to stdout, which would otherwise bypass
// the structured stream entirely. HAP debug output is verbose and only worth
// enabling when chasing a pairing problem.
func BridgeHAP(logger *slog.Logger, debug bool) {
	hapLogger := logger.With(slog.String("component", "hap"))

	// Drop the stdlib prefix and timestamp; slog adds its own.
	haplog.Info.SetFlags(0)
	haplog.Info.SetPrefix("")
	haplog.Info.SetOutput(&slogWriter{logger: hapLogger, level: slog.LevelInfo})

	haplog.Debug.SetFlags(0)
	haplog.Debug.SetPrefix("")
	if debug {
		haplog.Debug.SetOutput(&slogWriter{logger: hapLogger, level: slog.LevelDebug})
	} else {
		haplog.Debug.SetOutput(io.Discard)
	}
}

// slogWriter turns each line written to it into one slog record.
type slogWriter struct {
	logger *slog.Logger
	level  slog.Level
}

func (w *slogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimRight(string(p), "\n")
	if msg != "" {
		w.logger.Log(context.Background(), w.level, msg)
	}
	return len(p), nil
}
