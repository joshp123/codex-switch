package store

import (
	"math"
	"path/filepath"
	"testing"
	"time"
)

func TestWeeklyAllowanceBurn(t *testing.T) {
	now := time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour
	reading := func(account AccountKey, daysAgo float64, window int, used float64) WeeklyAllowanceReading {
		return WeeklyAllowanceReading{
			AccountKey:  account,
			ObservedAt:  now.Add(-time.Duration(daysAgo * float64(day))),
			ResetsAt:    now.Add(time.Duration(window) * 7 * day),
			UsedPercent: used,
		}
	}
	for _, test := range []struct {
		name           string
		readings       []WeeklyAllowanceReading
		want           float64
		visible        bool
		storedReadings int
	}{
		{
			name: "reset preserves consumed allowance and counts new window usage",
			readings: []WeeklyAllowanceReading{
				reading("first", 7, 0, 10), reading("first", 4, 0, 70),
				reading("first", 3, 1, 5), reading("first", 0, 1, 60),
			},
			want: 1.2, visible: true,
		},
		{
			name: "a reset reported a minute apart is the same window",
			readings: []WeeklyAllowanceReading{
				reading("first", 2, 1, 80),
				{AccountKey: "first", ObservedAt: now.Add(-day), ResetsAt: now.Add(7*day - time.Minute), UsedPercent: 85},
				reading("first", 0, 1, 90),
			},
			want: 0.1 * 7 / 2, visible: true,
		},
		{
			name: "early reset does not project the old window",
			readings: []WeeklyAllowanceReading{
				reading("first", 2, 1, 0), reading("first", 1, 1, 69),
				reading("first", 0.5, 2, 3), reading("first", 0, 2, 10),
			},
			want: 0.79 * 7 / 2, visible: true,
		},
		{
			name: "gaps use observed differences with a baseline before seven days",
			readings: []WeeklyAllowanceReading{
				reading("first", 8, 1, 10), reading("first", 6, 1, 30), reading("first", 0, 1, 50),
			},
			want: 0.4, visible: true,
		},
		{
			name: "changes before seven days do not count",
			readings: []WeeklyAllowanceReading{
				reading("first", 8, 1, 10), reading("first", 7.5, 1, 30), reading("first", 1, 1, 50),
			},
			want: 0.2, visible: true,
		},
		{
			name: "a drop within a window counts only usage above its earlier high",
			readings: []WeeklyAllowanceReading{
				reading("first", 7, 1, 60), reading("first", 6, 1, 40),
				reading("first", 4, 1, 50), reading("first", 0, 1, 70),
			},
			want: 0.1, visible: true,
		},
		{
			name: "two sources alternating a point apart add nothing",
			readings: []WeeklyAllowanceReading{
				reading("first", 7, 1, 87), reading("first", 6, 1, 88), reading("first", 5, 1, 87),
				reading("first", 4, 1, 88), reading("first", 3, 1, 87), reading("first", 2, 1, 89),
				reading("first", 1, 1, 88), reading("first", 0, 1, 90),
			},
			want: 0.03, visible: true,
		},
		{
			name: "provider coverage scales the sum once and excludes new account baseline",
			readings: []WeeklyAllowanceReading{
				reading("first", 2, 1, 10), reading("first", 0, 1, 30),
				reading("second", 1, 1, 80), reading("second", 0, 1, 90),
			},
			want: 0.3 * 7 / 2, visible: true,
		},
		{
			name: "readings older than eight days are pruned",
			readings: []WeeklyAllowanceReading{
				reading("first", 9, 1, 10), reading("first", 2, 1, 20), reading("first", 0, 1, 30),
			},
			want: 0.1 * 7 / 2, visible: true, storedReadings: 2,
		},
		{
			name: "under one day has no burn",
			readings: []WeeklyAllowanceReading{
				reading("first", 0.5, 1, 20), reading("first", 0, 1, 30),
			},
		},
		{
			name: "unchanged allowance has zero burn after one day",
			readings: []WeeklyAllowanceReading{
				reading("first", 1, 1, 20), reading("first", 0, 1, 20),
			},
			visible: true, storedReadings: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			database, err := OpenStoreDatabase(filepath.Join(t.TempDir(), "allowance.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer database.CloseStoreDatabase()
			rememberTestAccounts(t, database, "first", "second")
			for _, sample := range test.readings {
				if err := database.RecordWeeklyAllowanceReading(sample); err != nil {
					t.Fatal(err)
				}
			}
			wantStoredReadings := test.storedReadings
			if wantStoredReadings == 0 {
				wantStoredReadings = len(test.readings)
			}
			var storedReadings int
			if err := database.SQLDatabase().QueryRow(`SELECT COUNT(*) FROM weekly_allowance_readings`).Scan(&storedReadings); err != nil {
				t.Fatal(err)
			}
			if storedReadings != wantStoredReadings {
				t.Fatalf("stored readings = %d, want %d", storedReadings, wantStoredReadings)
			}
			burn, err := database.ReadWeeklyAllowanceBurn([]AccountKey{"first", "second"}, now)
			if err != nil {
				t.Fatal(err)
			}
			if (burn != nil) != test.visible {
				t.Fatalf("burn visibility = %v, want %v", burn != nil, test.visible)
			}
			if burn != nil && math.Abs(*burn-test.want) > 1e-9 {
				t.Errorf("burn = %g, want %g", *burn, test.want)
			}
		})
	}
}
