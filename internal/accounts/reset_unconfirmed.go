package accounts

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/joshp123/codex-switch/internal/logtime"
	"github.com/joshp123/codex-switch/internal/store"
)

// unconfirmedReset is a credit this installation asked OpenAI to spend, kept
// until OpenAI's usage reading shows the reset. OpenAI applies a spend at
// once, but its usage reading can still show the account at its limit for a
// while; deciding from that reading would spend a second credit for the same
// need. So while a reset is unconfirmed, this installation spends nothing,
// however long the reading takes. It is written before the request is sent
// and removed when OpenAI answers that it spent nothing, or when the reading
// shows the reset.
type unconfirmedReset struct {
	AccountKey store.AccountKey
	CreditID   string
	// AskedAt is no later than OpenAI applied the reset.
	AskedAt time.Time
}

const unconfirmedResetFilename = "unconfirmed-reset.json"

// readingLagWarning is how long an unconfirmed reset waits before the log says
// that OpenAI's reading has not caught up.
const readingLagWarning = 30 * time.Minute

func readUnconfirmedReset(directory string) (*unconfirmedReset, error) {
	data, err := os.ReadFile(filepath.Join(directory, unconfirmedResetFilename))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var reset unconfirmedReset
	if err := json.Unmarshal(data, &reset); err != nil {
		return nil, fmt.Errorf("read unconfirmed reset: %w", err)
	}
	return &reset, nil
}

func writeUnconfirmedReset(directory string, reset unconfirmedReset) error {
	data, err := json.Marshal(reset)
	if err != nil {
		return err
	}
	return writeAtomicData(filepath.Join(directory, unconfirmedResetFilename), data)
}

func removeUnconfirmedReset(directory string) error {
	if err := os.Remove(filepath.Join(directory, unconfirmedResetFilename)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// waitingFor says why no credit may be spent yet, or "" once the reset's
// account has left the pool or its reading shows the reset.
func (reset unconfirmedReset) waitingFor(readings []codexAccountReading, now time.Time) string {
	for _, reading := range readings {
		if reading.account.Key != reset.AccountKey {
			continue
		}
		name := reading.account.DisplayName
		switch {
		case reading.signedOut:
			return fmt.Sprintf("No reset: %s, whose reset credit %s was spent at %s, is signed out, so OpenAI's usage reading cannot show the reset.",
				name, reset.CreditID, logtime.Format(reset.AskedAt))
		case readingShowsReset(reading.allowance, reset.AskedAt):
			return ""
		case now.Sub(reset.AskedAt) >= readingLagWarning:
			return fmt.Sprintf("No reset: OpenAI's usage reading still does not show the reset of %s, whose reset credit %s was spent at %s. "+
				"No credit is spent until it does, or until that account's allowance resets by itself.", name, reset.CreditID, logtime.Format(reset.AskedAt))
		default:
			return fmt.Sprintf("No reset: OpenAI's usage reading does not show the reset of %s yet, whose reset credit %s was spent at %s.",
				name, reset.CreditID, logtime.Format(reset.AskedAt))
		}
	}
	return ""
}

// readingShowsReset reports whether an account's reading was taken after a
// reset asked for at askedAt. A reset starts every allowance window again:
// OpenAI reports a window that has not started as beginning at the read, and
// starts it at the account's first request. A reading from before the reset
// still shows a window that began earlier. OpenAI's window ends move by a
// second or two between reads, so a window counts as begun after the reset
// from a minute before it.
func readingShowsReset(sample AllowanceSample, askedAt time.Time) bool {
	for _, window := range sample.Windows {
		if window.ModelID != "" || window.ModelName != "" {
			continue
		}
		began := window.ResetsAt.Add(-time.Duration(window.DurationSeconds) * time.Second)
		if began.Before(askedAt.Add(-time.Minute)) {
			return false
		}
	}
	return true
}
