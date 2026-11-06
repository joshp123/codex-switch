package store

import (
	"fmt"
	"math"
	"slices"
	"testing"
	"time"
)

func readWeeklyAllowanceBurnWithoutCache(database *StoreDatabase, accountKeys []AccountKey, now time.Time) (*float64, error) {
	selectedAccounts := make(map[AccountKey]bool, len(accountKeys))
	for _, accountKey := range accountKeys {
		selectedAccounts[accountKey] = true
	}
	rows, err := database.sqlDatabase.Query(`
SELECT account_key, resets_at_unix, used_percent, observed_at_unix_nano
FROM weekly_allowance_readings
WHERE observed_at_unix_nano >= ? AND observed_at_unix_nano <= ?
ORDER BY account_key, observed_at_unix_nano`, now.Add(-8*24*time.Hour).UnixNano(), now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("read weekly allowance burn: %w", err)
	}
	defer rows.Close()
	var earliestObservation time.Time
	var previous WeeklyAllowanceReading
	// windowHigh is the highest reading of the account's current window. Two
	// sources report the same window with different rounding and lag (Claude's
	// usage API and its response headers), so readings within a window go up
	// and down; only usage above the window's earlier high is new.
	var windowHigh float64
	var usedPercent float64
	for rows.Next() {
		var reading WeeklyAllowanceReading
		var resetsAt, observedAt int64
		if err := rows.Scan(&reading.AccountKey, &resetsAt, &reading.UsedPercent, &observedAt); err != nil {
			return nil, err
		}
		if !selectedAccounts[reading.AccountKey] {
			continue
		}
		reading.ResetsAt, reading.ObservedAt = time.Unix(resetsAt, 0), time.Unix(0, observedAt)
		if earliestObservation.IsZero() || reading.ObservedAt.Before(earliestObservation) {
			earliestObservation = reading.ObservedAt
		}
		if previous.AccountKey == reading.AccountKey && !reading.ObservedAt.Before(now.Add(-allowanceWeek)) {
			increase := reading.UsedPercent
			// Providers report the same reset a second or a minute apart; only a
			// reset that moves by more than that starts a new window.
			if sameAllowanceWindow(previous, reading) {
				increase -= windowHigh
			}
			usedPercent += math.Max(0, increase)
		}
		if previous.AccountKey == reading.AccountKey && sameAllowanceWindow(previous, reading) {
			windowHigh = math.Max(windowHigh, reading.UsedPercent)
		} else {
			windowHigh = reading.UsedPercent
		}
		previous = reading
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if earliestObservation.IsZero() || now.Sub(earliestObservation) < 24*time.Hour {
		return nil, nil
	}
	coveredFor := min(now.Sub(earliestObservation), allowanceWeek)
	accounts := usedPercent / 100 * float64(allowanceWeek) / float64(coveredFor)
	return &accounts, nil
}

func TestWeeklyAllowanceBurnCacheWindowBoundaries(t *testing.T) {
	database, err := OpenStoreDatabase(t.TempDir() + "/burn.db")
	if err != nil {
		t.Fatal(err)
	}
	defer database.CloseStoreDatabase()
	start := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	names := []AccountKey{"first", "second", "third"}
	rememberTestAccounts(t, database, names...)
	times := []time.Time{start}
	for index := 0; index < 30; index++ {
		for accountIndex, name := range names {
			observed := start.Add(time.Duration(index)*9*time.Hour + time.Duration(accountIndex)*time.Minute + time.Duration(index)*time.Nanosecond)
			reset := start.Add(time.Duration(index/19)*allowanceWeek + time.Duration(index%3)*time.Second)
			used := float64((index*17+accountIndex*3)%101) / 3
			if _, err := database.sqlDatabase.Exec(`INSERT INTO weekly_allowance_readings (account_name, account_key, resets_at_unix, used_percent, observed_at_unix_nano) VALUES(?,?,?,?,?)`, "account-"+string(name), name, reset.Unix(), used, observed.UnixNano()); err != nil {
				t.Fatal(err)
			}
			for _, age := range []time.Duration{0, 24 * time.Hour, allowanceWeek, 8 * 24 * time.Hour} {
				for _, edge := range []time.Duration{-time.Nanosecond, 0, time.Nanosecond} {
					times = append(times, observed.Add(age+edge))
				}
			}
		}
	}
	slices.SortFunc(times, func(a, b time.Time) int { return a.Compare(b) })
	selections := [][]AccountKey{names, {"second"}, {"third", "first", "first"}, {"missing"}, nil}
	for _, now := range times {
		for _, selection := range selections {
			assertWeeklyBurnMatchesUncached(t, database, selection, now)
			if len(database.weeklyAllowanceBurns) > 2 {
				t.Fatal("cache retained historical account selections")
			}
		}
	}
	now := times[len(times)-1]
	if _, err := database.sqlDatabase.Exec(`INSERT INTO weekly_allowance_readings (account_name, account_key, resets_at_unix, used_percent, observed_at_unix_nano) VALUES(?,?,?,?,?)`, "account-1", "first", now.Add(allowanceWeek).Unix(), 92.25, now.UnixNano()); err != nil {
		t.Fatal(err)
	}
	assertWeeklyBurnMatchesUncached(t, database, names, now)
	assertWeeklyBurnMatchesUncached(t, database, names, start.Add(2*24*time.Hour))
}

func assertWeeklyBurnMatchesUncached(t *testing.T, database *StoreDatabase, selection []AccountKey, now time.Time) {
	t.Helper()
	want, err := readWeeklyAllowanceBurnWithoutCache(database, selection, now)
	if err != nil {
		t.Fatal(err)
	}
	got, err := database.ReadWeeklyAllowanceBurn(selection, now)
	if err != nil {
		t.Fatal(err)
	}
	if (got == nil) != (want == nil) {
		t.Fatalf("at %s: visibility got %v want %v", now, got, want)
	}
	if got != nil && math.Float64bits(*got) != math.Float64bits(*want) {
		t.Fatalf("at %s: got %.17g want %.17g", now, *got, *want)
	}
}
