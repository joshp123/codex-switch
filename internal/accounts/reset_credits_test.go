package accounts

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/joshp123/codex-switch/internal/store"
)

const week = 7 * 24 * time.Hour

// resetTestReading is an account whose week resets at resetsAt with used
// percent used, holding credits that expire after the given durations.
func resetTestReading(name string, folder string, used float64, resetsAt time.Time, now time.Time, creditExpiries ...time.Duration) codexAccountReading {
	reading := codexAccountReading{
		account: PoolAccount{Key: store.AccountKey("chatgpt:" + name + "@example.test"), Folder: folder, DisplayName: name},
		allowance: AllowanceSample{Allowed: used < 100, LimitReached: used >= 100, ObservedAt: now,
			Windows: []ReportedWindow{{UsedPercent: used, DurationSeconds: int64(week / time.Second), ResetsAt: resetsAt}}},
	}
	for index, expiry := range creditExpiries {
		reading.credits = append(reading.credits, ResetCredit{ID: name + "-credit-" + string(rune('a'+index)), Status: "available",
			ResetType: "codex_rate_limits", ExpiresAt: now.Add(expiry)})
	}
	return reading
}

func TestPoolOutSpendsTheCreditOfTheAccountThatRefillsLast(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	readings := []codexAccountReading{
		resetTestReading("one", "account-1", 100, now.Add(24*time.Hour), now, 3*24*time.Hour),
		// The soonest-expiring credit is here, but this account refills before the next.
		resetTestReading("two", "account-2", 100, now.Add(48*time.Hour), now, 24*time.Hour),
		resetTestReading("three", "account-3", 100, now.Add(72*time.Hour), now, 5*24*time.Hour, 2*24*time.Hour),
	}
	for _, order := range [][]int{{0, 1, 2}, {2, 1, 0}, {1, 2, 0}} {
		var ordered []codexAccountReading
		for _, index := range order {
			ordered = append(ordered, readings[index])
		}
		choice, found, reason := chooseResetCredit(ordered, now, resetRules{whenPoolOut: true})
		if !found || choice.credit.ID != "three-credit-b" {
			t.Fatalf("order %v: chose %q (found %t, %s); want the earliest credit of the account that refills last", order, choice.credit.ID, found, reason)
		}
	}
}

func TestPoolOutSpendsNothingWhenWorkCanContinueOrWaitBriefly(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	for name, test := range map[string]struct {
		second codexAccountReading
		want   string
	}{
		"an account can take work":         {resetTestReading("two", "account-2", 99, now.Add(48*time.Hour), now, time.Hour), "two can take work"},
		"an account refills within 30 min": {resetTestReading("two", "account-2", 100, now.Add(29*time.Minute), now, time.Hour), "within 30 minutes"},
		"no account has a credit":          {resetTestReading("two", "account-2", 100, now.Add(48*time.Hour), now), "no Codex account has a reset credit"},
	} {
		t.Run(name, func(t *testing.T) {
			readings := []codexAccountReading{resetTestReading("one", "account-1", 100, now.Add(24*time.Hour), now), test.second}
			if _, found, reason := chooseResetCredit(readings, now, resetRules{whenPoolOut: true}); found || !strings.Contains(reason, test.want) {
				t.Fatalf("found %t, reason %q; want no spend because %s", found, reason, test.want)
			}
		})
	}
}

func TestExpiringCreditIsSpentOnlyWhenTheResetGainsAllowance(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	halfWeekLeft := now.Add(week / 2)
	for name, test := range map[string]struct {
		used   float64
		expiry time.Duration
		spend  bool
		reason string
	}{
		"used beyond the share of the week passed": {used: 60, expiry: 30 * time.Minute, spend: true},
		"at OpenAI's minimum":                      {used: 20, expiry: 30 * time.Minute, reason: "OpenAI resets only above 20%"},
		"behind the week's pace":                   {used: 40, expiry: 30 * time.Minute, reason: "no more than the 50% of the week that has passed"},
		"more than an hour left":                   {used: 60, expiry: 61 * time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			readings := []codexAccountReading{resetTestReading("one", "account-1", test.used, halfWeekLeft, now, test.expiry)}
			choice, found, reason := chooseResetCredit(readings, now, resetRules{beforeExpiry: true})
			if found != test.spend || !strings.Contains(reason, test.reason) || (test.reason == "" && !test.spend && reason != "") {
				t.Fatalf("found %t (%q), reason %q; want spend %t, reason %q", found, choice.credit.ID, reason, test.spend, test.reason)
			}
		})
	}
}

func TestExpiringCreditsAreSpentEarliestFirst(t *testing.T) {
	now := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	readings := []codexAccountReading{
		resetTestReading("one", "account-1", 80, now.Add(week/2), now, 50*time.Minute),
		resetTestReading("two", "account-2", 80, now.Add(week/2), now, 10*time.Minute),
	}
	for range 2 {
		choice, found, _ := chooseResetCredit(readings, now, resetRules{beforeExpiry: true})
		if !found || choice.credit.ID != "two-credit-a" {
			t.Fatalf("chose %q; want the credit that expires first", choice.credit.ID)
		}
		slices.Reverse(readings)
	}
}
