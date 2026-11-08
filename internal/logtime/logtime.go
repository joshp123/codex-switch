// Package logtime is how the proxy writes a time: in UTC, with the zone
// marked, as RFC 3339. A Go process reads the Mac's time zone once, when it
// starts, so a local time written after the Mac changes zone would be wrong.
// Status and the menu run in the reader's process and show local time.
package logtime

import (
	"io"
	"time"
)

// Format writes at as the proxy log does, for example 2026-11-07T22:00:00Z.
func Format(at time.Time) string {
	return at.UTC().Format(time.RFC3339)
}

// StampedLines starts each line the standard logger writes with the time it
// was written. Set the logger's flags to 0 so it adds no timestamp of its own.
type StampedLines struct{ Output io.Writer }

func (stamped StampedLines) Write(line []byte) (int, error) {
	if _, err := stamped.Output.Write(append([]byte(Format(time.Now())+" "), line...)); err != nil {
		return 0, err
	}
	return len(line), nil
}
