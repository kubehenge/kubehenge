//go:build !linux

package appliance

import (
	"context"
	"log/slog"
)

// systemAnnouncer is nil off Linux, where there is no appliance to announce.
func systemAnnouncer(context.Context, *slog.Logger) *Announcer { return nil }
