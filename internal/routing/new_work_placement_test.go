package routing

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/store"
)

func TestNewWorkUsesTheAllowanceThatResetsFirstAndStartsIdleWeeks(t *testing.T) {
	database, err := store.OpenStoreDatabase(filepath.Join(t.TempDir(), "routing.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.CloseStoreDatabase()

	now := time.Now()
	week := 7 * 24 * time.Hour
	weekly := func(used float64, resetsAt time.Time) accounts.AllowanceSample {
		return accounts.AllowanceSample{Allowed: true, ObservedAt: now, Windows: []accounts.ReportedWindow{
			{UsedPercent: used, DurationSeconds: int64(week.Seconds()), ResetsAt: resetsAt},
		}}
	}
	facts := capacityWaitFacts{}
	for _, name := range []store.AccountName{"account-1", "account-2"} {
		facts = append(facts, accounts.AccountRoutingFact{
			AccountName: name, DisplayName: string(name), Credentials: accounts.Credentials{AccessToken: "test", AccountID: string(name)},
			AccountState: accounts.AccountReady, SupportedModelSlugs: []string{"luna"}, RefreshedAt: now,
		})
	}
	router := CreateTaskRouter(database, facts)
	route := func(taskID store.TaskID, want store.AccountName) RouteDecision {
		t.Helper()
		decision, err := router.RouteForTask(taskID, store.ModelTurnWorkKind, "luna")
		if err != nil || decision.AccountName != want {
			t.Fatalf("task %q went to %s (%v); want %s", taskID, decision.AccountName, err, want)
		}
		return decision
	}

	// Account 1 has less left, but its allowance resets first.
	facts[0].AllowanceSample = weekly(70, now.Add(24*time.Hour))
	facts[1].AllowanceSample = weekly(10, now.Add(5*24*time.Hour))
	route("first", "account-1")
	route("", "account-1")
	if _, pinned, err := database.ReadTaskAccountPin(""); err != nil || pinned {
		t.Fatalf("model work without a task ID was pinned: pinned=%v, error=%v", pinned, err)
	}

	// Account 2's allowance reset and nothing has used it since, so its week has
	// not started. New work starts it; the pinned task stays where it is.
	facts[1].AllowanceSample = weekly(0, now.Add(week))
	decision := route("second", "account-2")
	if decision.Assignment != "new task assigned to account-2, whose weekly allowance has not started" {
		t.Fatalf("assignment = %q", decision.Assignment)
	}
	route("first", "account-1")

	// Allowances that reset together go to the account with more left.
	facts[0].AllowanceSample = weekly(50, now.Add(24*time.Hour))
	facts[1].AllowanceSample = weekly(40, now.Add(24*time.Hour))
	route("third", "account-2")

	if _, err := router.RouteForTask("metadata", store.TaskMetadataWorkKind, "luna"); err != nil {
		t.Fatal(err)
	}
	if _, pinned, err := database.ReadTaskAccountPin("metadata"); err != nil || pinned {
		t.Fatalf("metadata created a task pin: pinned=%v, error=%v", pinned, err)
	}
}
