package bridgerpc

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/btcsuite/btclog/v2"
	"github.com/lightningnetwork/lnd/build"
)

// log is a logger that is initialized with no output filters. This means the
// package will not perform any logging by default until the caller requests
// it.
var log btclog.Logger

// Subsystem defines the logging code for this subsystem.
const Subsystem = "BRPC"

// The default amount of logging is none.
func init() {
	UseLogger(build.NewSubLogger(Subsystem, nil))
}

// DisableLog disables all library log output. Logging output is disabled by
// default until UseLogger is called.
func DisableLog() {
	UseLogger(btclog.Disabled)
}

// UseLogger uses a specified Logger to output package logging info. This
// should be used in preference to SetLogWriter if the caller is also using
// btclog.
func UseLogger(logger btclog.Logger) {
	log = logger
}

// bridgeLogger adapts this package's logger to the slog.Logger the swap
// packages take.
//
// They are a separate module with no reason to know about btclog, and this is
// the whole of the adaptation: everything they log arrives here and goes
// through the node's own logging, rotation and level settings, rather than to
// a second destination an operator would have to find.
func bridgeLogger(direction string) *slog.Logger {
	return slog.New(bridgeLogHandler{}).With("direction", direction)
}

// bridgeLogHandler forwards slog records to this package's logger.
type bridgeLogHandler struct {
	attrs []slog.Attr
}

// Enabled asks the node's logger, so the bridge follows the level an operator
// set rather than one of its own.
func (h bridgeLogHandler) Enabled(_ context.Context, l slog.Level) bool {
	switch {
	case l >= slog.LevelError:
		return log.Level() <= btclog.LevelError
	case l >= slog.LevelWarn:
		return log.Level() <= btclog.LevelWarn
	case l >= slog.LevelInfo:
		return log.Level() <= btclog.LevelInfo
	default:
		return log.Level() <= btclog.LevelDebug
	}
}

// Handle renders a record through the node's logger.
func (h bridgeLogHandler) Handle(_ context.Context, r slog.Record) error {
	var sb strings.Builder
	sb.WriteString(r.Message)

	for _, a := range h.attrs {
		fmt.Fprintf(&sb, " %s=%v", a.Key, a.Value)
	}
	r.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&sb, " %s=%v", a.Key, a.Value)

		return true
	})

	switch {
	case r.Level >= slog.LevelError:
		log.Error(sb.String())
	case r.Level >= slog.LevelWarn:
		log.Warn(sb.String())
	case r.Level >= slog.LevelInfo:
		log.Info(sb.String())
	default:
		log.Debug(sb.String())
	}

	return nil
}

// WithAttrs returns a handler carrying the extra attributes.
func (h bridgeLogHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	out := bridgeLogHandler{attrs: make([]slog.Attr, 0,
		len(h.attrs)+len(attrs))}
	out.attrs = append(out.attrs, h.attrs...)
	out.attrs = append(out.attrs, attrs...)

	return out
}

// WithGroup is not used by the swap packages, so grouping is flattened rather
// than implemented with a nesting scheme nothing would exercise.
func (h bridgeLogHandler) WithGroup(string) slog.Handler { return h }
