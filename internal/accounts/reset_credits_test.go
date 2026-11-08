package accounts

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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

// Another Mac spends the credit of the account that refills last while this
// Mac is reading that account. This Mac must see the spend as an account that
// can take work and spend nothing, never as a missing credit beside an account
// still at its limit, which would lead it to spend a second credit.
func TestAnotherMacsSpendDuringTheReadsMakesThisMacBackOff(t *testing.T) {
	data := t.TempDir()
	late := writeAccountFixture(t, filepath.Join(data, "account-1"), "late", "late@example.test", unexpiringAccessToken("late"))
	writeAccountFixture(t, filepath.Join(data, "account-2"), "early", "early@example.test", unexpiringAccessToken("early"))
	now := time.Now()
	var mutex sync.Mutex
	otherMacSpent := false
	var spendRequests []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mutex.Lock()
		defer mutex.Unlock()
		isLate := r.Header.Get("ChatGPT-Account-ID") == late.AccountID
		refillsAt, credit, status := now.Add(24*time.Hour), "early-credit", "available"
		if isLate {
			refillsAt, credit = now.Add(72*time.Hour), "late-credit"
		}
		spent := isLate && otherMacSpent
		if spent {
			status = "redeemed"
		}
		switch r.URL.Path {
		case "/backend-api/wham/rate-limit-reset-credits":
			fmt.Fprintf(w, `{"credits":[{"id":%q,"status":%q,"reset_type":"codex_rate_limits","expires_at":%q}]}`, credit, status, now.Add(10*24*time.Hour).Format(time.RFC3339))
		case "/backend-api/wham/usage":
			used, resetAt := 100, refillsAt
			if spent {
				used, resetAt = 0, now.Add(week)
			}
			fmt.Fprintf(w, `{"plan_type":"pro","rate_limit":{"allowed":%t,"limit_reached":%t,"primary_window":{"used_percent":%d,"limit_window_seconds":604800,"reset_at":%d}}}`,
				used < 100, used >= 100, used, resetAt.Unix())
		case "/backend-api/wham/rate-limit-reset-credits/consume":
			body, _ := io.ReadAll(r.Body)
			spendRequests = append(spendRequests, string(body))
			_, _ = w.Write([]byte(`{"code":"reset"}`))
		default:
			http.NotFound(w, r)
		}
		// The other Mac's spend lands right after this Mac's first read of the account.
		otherMacSpent = otherMacSpent || isLate
	}))
	defer upstream.Close()
	origin, _ := url.Parse(upstream.URL)
	decision, err := spendResetCredit(context.Background(), data, origin, upstream.Client(), resetRules{whenPoolOut: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(spendRequests) != 0 || !strings.Contains(decision.Message, "can take work") {
		t.Fatalf("spent %v (%q); want no spend, because the account the other Mac reset can take work", spendRequests, decision.Message)
	}
}
