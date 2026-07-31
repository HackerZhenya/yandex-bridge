package bridge

import (
	"log/slog"
	"strings"
)

// LogInventory prints what the bridge found and what it did with it.
//
// This exists because writing config.yaml is otherwise guesswork: the device
// ids are opaque UUIDs that appear nowhere in the Home app, and a capability
// the bridge cannot map disappears without trace. One line per device, at info
// level, means the answer is in the logs of any normal run rather than behind
// a debug flag.
func LogInventory(logger *slog.Logger, reports []MappingReport) {
	var exported, skipped, withUnmapped int

	for _, r := range reports {
		attrs := []any{
			slog.String("device_id", r.DeviceID),
			slog.String("name", r.Name),
			slog.String("type", string(r.Type)),
		}
		if r.Room != "" {
			attrs = append(attrs, slog.String("room", r.Room))
		}

		if r.Skipped {
			skipped++
			attrs = append(attrs, slog.String("reason", r.Reason))
			logger.Info("device not exported", attrs...)
			continue
		}

		exported++
		attrs = append(attrs,
			slog.String("homekit", string(r.Kind)),
			slog.String("mapped", strings.Join(r.Mapped, ", ")))
		if len(r.Unmapped) > 0 {
			withUnmapped++
			// Listed explicitly so it is obvious what the bridge is dropping —
			// a mode or a reading the user may well have been looking for.
			attrs = append(attrs, slog.String("unmapped", strings.Join(r.Unmapped, ", ")))
		}
		logger.Info("device exported", attrs...)
	}

	logger.Info("device inventory",
		slog.Int("total", len(reports)),
		slog.Int("exported", exported),
		slog.Int("skipped", skipped),
		slog.Int("with_unmapped_features", withUnmapped),
		slog.String("details", "GET /devices"))
}
