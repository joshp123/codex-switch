package accounts

import (
	"testing"
	"time"
)

func TestAnalyseUsesTheExactProviderWindow(t *testing.T) {
	start := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	value := sample(start.Add(2*time.Hour),
		ReportedWindow{UsedPercent: 40, DurationSeconds: 18000, ResetsAt: start.Add(5 * time.Hour)},
		ReportedWindow{UsedPercent: 21, DurationSeconds: 604800, ResetsAt: start.Add(7 * 24 * time.Hour)},
	)
	if got, known := analyse(value, AccountReady, FiveHours); !known || got.FreePercent != 60 || !got.ResetsAt.Equal(start.Add(5*time.Hour)) {
		t.Fatalf("5-hour window = %#v, %v", got, known)
	}
	if got, known := analyse(value, AccountReady, Weekly); !known || got.FreePercent != 79 {
		t.Fatalf("weekly window = %#v, %v", got, known)
	}
	atStart := sample(start, ReportedWindow{UsedPercent: 0, DurationSeconds: 18000, ResetsAt: start.Add(5 * time.Hour)})
	if got, known := analyse(atStart, AccountReady, FiveHours); !known || got.FreePercent != 100 {
		t.Fatalf("window start = %#v, %v", got, known)
	}
}

// OpenAI starts a window at the account's first request after a reset. Until
// then it reports nothing used and a reset a full window from the reading, a
// little over when its clock runs ahead.
func TestAnalyseRecognisesAWindowThatHasNotStarted(t *testing.T) {
	start := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	week := 7 * 24 * time.Hour
	for _, test := range []struct {
		name       string
		window     ReportedWindow
		notStarted bool
	}{
		{"idle", ReportedWindow{UsedPercent: 0, DurationSeconds: 604800, ResetsAt: start.Add(week)}, true},
		{"idle, OpenAI's clock ahead", ReportedWindow{UsedPercent: 0, DurationSeconds: 604800, ResetsAt: start.Add(week + time.Second)}, true},
		{"started, nothing used yet", ReportedWindow{UsedPercent: 0, DurationSeconds: 604800, ResetsAt: start.Add(week - time.Hour)}, false},
		{"used", ReportedWindow{UsedPercent: 1, DurationSeconds: 604800, ResetsAt: start.Add(week)}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, known := analyse(sample(start, test.window), AccountReady, Weekly)
			if !known || got.NotStarted != test.notStarted {
				t.Fatalf("weekly window = %#v, %v; want not started = %v", got, known, test.notStarted)
			}
		})
	}
}

func TestAnalyseRejectsMissingResetAndInvalidWindows(t *testing.T) {
	start := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		sample AllowanceSample
	}{
		{name: "not reported", sample: sample(start.Add(time.Hour), ReportedWindow{UsedPercent: 10, DurationSeconds: 604800, ResetsAt: start.Add(7 * 24 * time.Hour)})},
		{name: "already reset", sample: sample(start.Add(5*time.Hour), ReportedWindow{UsedPercent: 10, DurationSeconds: 18000, ResetsAt: start.Add(5 * time.Hour)})},
		{name: "negative use", sample: sample(start.Add(time.Hour), ReportedWindow{UsedPercent: -1, DurationSeconds: 18000, ResetsAt: start.Add(5 * time.Hour)})},
		{name: "use above 100", sample: sample(start.Add(time.Hour), ReportedWindow{UsedPercent: 101, DurationSeconds: 18000, ResetsAt: start.Add(5 * time.Hour)})},
		{name: "missing reset", sample: sample(start.Add(time.Hour), ReportedWindow{UsedPercent: 10, DurationSeconds: 18000})},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got, known := analyse(test.sample, AccountReady, FiveHours); known {
				t.Fatalf("window = %#v", got)
			}
		})
	}
}

func TestAnalyseTrustsOnlyAccountsWithAProviderReading(t *testing.T) {
	start := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	value := sample(start.Add(2*time.Hour), ReportedWindow{UsedPercent: 40, DurationSeconds: 18000, ResetsAt: start.Add(5 * time.Hour)})
	for _, state := range []AccountState{AccountUsageCheckFailed, AccountUsageCheckPending, AccountSignInRequired, AccountCredentialsUnavailable} {
		if got, known := analyse(value, state, FiveHours); known {
			t.Fatalf("state %s = %#v", state, got)
		}
	}
}

func TestAnalyseUsesAsOfOnlyToRejectAnExpiredRetainedSample(t *testing.T) {
	start := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	value := sample(start.Add(2*time.Hour), ReportedWindow{UsedPercent: 40, DurationSeconds: 18000, ResetsAt: start.Add(5 * time.Hour)})
	if got, known := AnalyseAllowance(value, AccountReady, start.Add(4*time.Hour), FiveHours); !known || got.FreePercent != 60 {
		t.Fatalf("current sample = %#v, %v", got, known)
	}
	if got, known := AnalyseAllowance(value, AccountReady, start.Add(5*time.Hour), FiveHours); known {
		t.Fatalf("expired retained sample = %#v", got)
	}
}

func sample(observedAt time.Time, windows ...ReportedWindow) AllowanceSample {
	return AllowanceSample{AccountID: "id-1", Plan: "pro", ObservedAt: observedAt, Windows: windows}
}

func analyse(value AllowanceSample, state AccountState, kind AllowanceWindowKind) (AllowanceWindow, bool) {
	return AnalyseAllowance(value, state, value.ObservedAt, kind)
}
