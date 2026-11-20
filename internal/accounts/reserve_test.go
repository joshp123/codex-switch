package accounts

import (
	"testing"
	"time"
)

func TestReserveAppliesOnlyToOverallWindows(t *testing.T) {
	now := time.Now()
	for _, window := range []ReportedWindow{
		{DurationSeconds: 5 * 3600},
		{DurationSeconds: 7 * 86400},
	} {
		window.ResetsAt = now.Add(time.Hour)
		fact := AccountRoutingFact{Reserve: AccountReserve{SoftPercent: 15, HardPercent: 5}, AllowanceSample: AllowanceSample{Windows: []ReportedWindow{window}}}
		for _, usage := range []struct {
			used float64
			soft bool
			held bool
		}{{84.9, false, false}, {85, true, false}, {94.9, true, false}, {95, true, true}, {95.1, true, true}} {
			fact.AllowanceSample.Windows[0].UsedPercent = usage.used
			if soft := SoftReserveReached(fact, now); soft != usage.soft {
				t.Fatalf("window %+v at %.1f%% used: soft reserve=%v", window, usage.used, soft)
			}
			if held := ReserveUnavailableReason(fact, now) != ""; held != usage.held {
				t.Fatalf("window %+v at %.1f%% used: held=%v", window, usage.used, held)
			}
		}
		fact.Reserve = AccountReserve{}
		if reason := ReserveStatus(fact, now); reason != "" {
			t.Fatalf("disabled reserve: %s", reason)
		}
		fact.Reserve = AccountReserve{SoftPercent: 15, HardPercent: 5}
		if reason := ReserveStatus(fact, window.ResetsAt); reason != "" {
			t.Fatalf("expired window: %s", reason)
		}
	}

	for _, subLimit := range []ReportedWindow{
		{DurationSeconds: 7 * 86400, ModelName: "Fable", UsedPercent: 95, ResetsAt: now.Add(time.Hour)},
		{DurationSeconds: 7 * 86400, ModelName: "Opus", UsedPercent: 100, ResetsAt: now.Add(time.Hour)},
		{DurationSeconds: 7 * 86400, ModelID: "claude-fable-5-2", UsedPercent: 98, ResetsAt: now.Add(time.Hour)},
	} {
		fact := AccountRoutingFact{
			Reserve: AccountReserve{SoftPercent: 15, HardPercent: 5},
			AllowanceSample: AllowanceSample{Windows: []ReportedWindow{
				{DurationSeconds: 7 * 86400, UsedPercent: 60, ResetsAt: now.Add(time.Hour)},
				subLimit,
			}},
		}
		if SoftReserveReached(fact, now) || ReserveUnavailableReason(fact, now) != "" || ReserveStatus(fact, now) != "" {
			t.Fatalf("model sub-limit %+v triggered account reserve: status=%q", subLimit, ReserveStatus(fact, now))
		}
	}
}
