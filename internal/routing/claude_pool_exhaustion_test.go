package routing

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/store"
)

// The pool on 31 October 2026: Claude 1 held back by its 10% reserve until its
// weekly reset, Claude 2 out of its weekly allowance until later, and Claude 3
// out of its five-hour allowance with its weekly reserve also reached.
func TestExhaustedClaudePoolNamesTheFirstResetThatFreesAnAccount(t *testing.T) {
	database, err := store.OpenStoreDatabase(filepath.Join(t.TempDir(), "pool.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.CloseStoreDatabase()
	now := time.Now().UTC().Truncate(time.Second)
	claudeAccount := func(name store.AccountName, reserve int, windows ...accounts.ReportedWindow) accounts.AccountRoutingFact {
		return accounts.AccountRoutingFact{
			Provider: accounts.ProviderClaude, AccountName: name, DisplayName: string(name), AccountState: accounts.AccountReady,
			Credentials:     accounts.Credentials{AccountID: string(name), AccessToken: "test"},
			Reserve:         accounts.AccountReserve{SoftPercent: reserve, HardPercent: reserve},
			AllowanceSample: accounts.AllowanceSample{Windows: windows},
		}
	}
	weekly := func(used float64, resetsIn time.Duration) accounts.ReportedWindow {
		return accounts.ReportedWindow{DurationSeconds: 7 * 86400, UsedPercent: used, ResetsAt: now.Add(resetsIn)}
	}
	fiveHour := func(used float64, resetsIn time.Duration) accounts.ReportedWindow {
		return accounts.ReportedWindow{DurationSeconds: 5 * 3600, UsedPercent: used, ResetsAt: now.Add(resetsIn)}
	}
	facts := capacityWaitFacts{
		claudeAccount("claude-1", 10, weekly(90, 8*time.Hour), fiveHour(20, 2*time.Hour)),
		claudeAccount("claude-2", 0, weekly(100, 25*time.Hour)),
		claudeAccount("claude-3", 10, weekly(95, 30*time.Hour), fiveHour(100, time.Hour)),
	}
	router := CreateTaskRouter(database, facts)
	exhaustion := func() *ClaudePoolExhaustedError {
		t.Helper()
		_, err := router.RouteForProviderTask("task", store.ModelTurnWorkKind, "claude-opus-5-5", accounts.ProviderClaude, "")
		var exhausted *ClaudePoolExhaustedError
		if !errors.As(err, &exhausted) || !errors.Is(err, ErrNoAvailableAccount) {
			t.Fatalf("route error = %v, want an exhausted Claude pool", err)
		}
		return exhausted
	}

	// Claude 3's five-hour reset comes first, but its reserve still holds it.
	if got := exhaustion(); !got.AvailableAgainAt.Equal(now.Add(8*time.Hour)) || got.LimitClaim != "seven_day" {
		t.Fatalf("available again at %s (%s), want Claude 1's weekly reset in 8h (seven_day)", got.AvailableAgainAt.Sub(now), got.LimitClaim)
	}

	facts[0] = claudeAccount("claude-1", 0, fiveHour(100, 3*time.Hour))
	facts[2].Reserve = accounts.AccountReserve{}
	if got := exhaustion(); !got.AvailableAgainAt.Equal(now.Add(time.Hour)) || got.LimitClaim != "five_hour" {
		t.Fatalf("available again at %s (%s), want Claude 3's five-hour reset in 1h", got.AvailableAgainAt.Sub(now), got.LimitClaim)
	}

	for index := range facts {
		facts[index].AccountState = accounts.AccountSignInRequired
	}
	if got := exhaustion(); !got.AvailableAgainAt.IsZero() {
		t.Fatalf("signed-out pool available again at %s; no reset can sign an account in", got.AvailableAgainAt)
	}
}
