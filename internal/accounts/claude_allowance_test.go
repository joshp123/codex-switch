package accounts

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The OAuth usage API reports percentages in limits[], while response headers
// report fractional utilization. A side request must retain the model window.
func TestClaudeScopedAllowanceSurvivesHaikuSideRequestAndProxyRestart(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "account-1"), 0700); err != nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse("https://api.anthropic.com")
	monitor := NewMonitor(data, origin, "", http.DefaultClient, monitorTestDatabase(t))
	monitor.monitoredAccountLocked(claudeTestAccount)
	var payload claudeUsageResponse
	if err := json.Unmarshal([]byte(`{"limits":[{"kind":"weekly_all","percent":14,"resets_at":"`+now.Add(7*24*time.Hour).Format(time.RFC3339)+`"},{"kind":"weekly_scoped","percent":100,"resets_at":"`+now.Add(7*24*time.Hour).Format(time.RFC3339)+`","scope":{"model":{"id":null,"display_name":"Fable"}}}]}`), &payload); err != nil {
		t.Fatal(err)
	}
	sample, err := payload.sample(PoolAccount{AccountID: "subscription", Plan: "Max 20x"}, now)
	if err != nil {
		t.Fatal(err)
	}
	monitor.monitoredAccountLocked(claudeTestAccount).fact = AccountRoutingFact{Provider: ProviderClaude, AccountState: AccountReady, AllowanceSample: sample}
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-7d-utilization", "0.15")
	headers.Set("anthropic-ratelimit-unified-7d-reset", strconv.FormatInt(now.Add(7*24*time.Hour).Unix(), 10))
	monitor.ObserveClaudeResponse(claudeTestAccount.Key, "claude-haiku-4-5", headers, 200)
	fact := monitor.accounts[claudeTestAccount.Key].fact
	if fact.AccountState != AccountReady {
		t.Fatalf("Fable window marked whole account %s", fact.AccountState)
	}
	if reason, _ := ClaudeUnavailableReason(fact, "claude-fable-5", now); reason == "" {
		t.Fatal("Haiku side request erased Fable limit")
	}
	if reason, _ := ClaudeUnavailableReason(fact, "claude-opus-4-8", now); reason != "" {
		t.Fatal("Fable limit blocked Opus")
	}
	restarted := NewMonitor(data, origin, "", http.DefaultClient, monitorTestDatabase(t))
	cache, _, err := restarted.reserveClaudeUsageRead(filepath.Join(data, "account-1"), now)
	if err != nil {
		t.Fatal(err)
	}
	fact.AllowanceSample = cache.Sample
	if reason, _ := ClaudeUnavailableReason(fact, "claude-fable-5", now); reason == "" {
		t.Fatal("proxy restart lost Fable window")
	}
}

func TestClaudeResponseModelBucketUsesRequestingModelAndPreservesSharedCapacity(t *testing.T) {
	now := time.Now()
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "account-1"), 0700); err != nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse("https://api.anthropic.com")
	monitor := NewMonitor(data, origin, "", http.DefaultClient, monitorTestDatabase(t))
	monitor.monitoredAccountLocked(claudeTestAccount)
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-7d_oi-utilization", "1")
	headers.Set("anthropic-ratelimit-unified-7d_oi-reset", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
	headers.Set("anthropic-ratelimit-unified-7d_oi-status", "rejected")
	monitor.ObserveClaudeResponse(claudeTestAccount.Key, "claude-fable-5", headers, 429)
	fact := monitor.accounts[claudeTestAccount.Key].fact
	if fact.AccountState != AccountReady {
		t.Fatal("model rejection changed shared availability")
	}
	if reason, _ := ClaudeUnavailableReason(fact, "claude-fable-5", now); reason == "" {
		t.Fatal("model bucket was not retained")
	}
	if reason, _ := ClaudeUnavailableReason(fact, "claude-opus-4-8", now); reason != "" {
		t.Fatal("model rejection blocked other model")
	}
	sonnetHeaders := make(http.Header)
	sonnetHeaders.Set("anthropic-ratelimit-unified-7d_sonnet-utilization", "1")
	sonnetHeaders.Set("anthropic-ratelimit-unified-7d_sonnet-reset", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
	monitor.ObserveClaudeResponse(claudeTestAccount.Key, "claude-haiku-4-5", sonnetHeaders, 200)
	fact = monitor.accounts[claudeTestAccount.Key].fact
	if reason, _ := ClaudeUnavailableReason(fact, "claude-sonnet-4-5", now); reason == "" {
		t.Fatal("named Sonnet bucket was attached to Haiku side request")
	}
	if reason, _ := ClaudeUnavailableReason(fact, "claude-haiku-4-5", now); reason != "" {
		t.Fatal("named Sonnet bucket blocked Haiku")
	}
}

func TestClaudeExactModelWindowAppliesToItsUndatedAlias(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "account-1"), 0700); err != nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse("https://api.anthropic.com")
	monitor := NewMonitor(data, origin, "", http.DefaultClient, monitorTestDatabase(t))
	monitor.monitoredAccountLocked(claudeTestAccount)
	fact := AccountRoutingFact{AllowanceSample: AllowanceSample{Windows: []ReportedWindow{{ModelID: "claude-opus-4-5-20251101", ModelName: "Opus", DurationSeconds: 7 * 86400, UsedPercent: 100, ResetsAt: now.Add(time.Hour)}}}}
	monitor.monitoredAccountLocked(claudeTestAccount).fact = fact
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-7d_oi-utilization", "1")
	headers.Set("anthropic-ratelimit-unified-7d_oi-status", "rejected")
	headers.Set("anthropic-ratelimit-unified-7d_oi-reset", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
	monitor.ObserveClaudeResponse(claudeTestAccount.Key, "claude-opus-4-5", headers, 429)
	fact = monitor.accounts[claudeTestAccount.Key].fact
	if reason, _ := ClaudeUnavailableReason(fact, "claude-opus-4-5", now); reason == "" {
		t.Fatal("undated alias bypassed exact model window")
	}
	if reason, _ := ClaudeUnavailableReason(fact, "claude-opus-4-8", now); reason != "" {
		t.Fatal("exact model window blocked another Opus version")
	}
}

func TestClaudeUsageBackoffIsSharedAcrossProxyProcesses(t *testing.T) {
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "account-1"), 0700); err != nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse("https://api.anthropic.com")
	first := NewMonitor(data, origin, "", http.DefaultClient, monitorTestDatabase(t))
	second := NewMonitor(data, origin, "", http.DefaultClient, monitorTestDatabase(t))
	now := time.Now()
	cache, due, err := first.reserveClaudeUsageRead(filepath.Join(data, "account-1"), now)
	if err != nil || !due {
		t.Fatalf("first read: due=%v err=%v", due, err)
	}
	cache.ReadAfter = now.Add(time.Hour)
	if err := first.saveClaudeAllowanceCache(filepath.Join(data, "account-1"), cache); err != nil {
		t.Fatal(err)
	}
	if _, due, err := second.reserveClaudeUsageRead(filepath.Join(data, "account-1"), now.Add(30*time.Minute)); err != nil || due {
		t.Fatalf("backoff was ignored: due=%v err=%v", due, err)
	}
	if _, due, err := second.reserveClaudeUsageRead(filepath.Join(data, "account-1"), now.Add(time.Hour)); err != nil || !due {
		t.Fatalf("backoff did not expire: due=%v err=%v", due, err)
	}
}

// On 31 October Anthropic answered one Opus request on each of two accounts
// with a 429 whose only deadline was midnight UTC, while both accounts had
// capacity and the same task ran on them again minutes later. A 429 that names
// no usage limit holds the account for its Retry-After, not until that reset.
func TestClaudeThrottleHoldsTheAccountOnlyUntilRetryAfter(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "account-1"), 0700); err != nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse("https://api.anthropic.com")
	monitor := NewMonitor(data, origin, "", http.DefaultClient, monitorTestDatabase(t))
	monitor.monitoredAccountLocked(claudeTestAccount)
	rejection := func(claim string, extra map[string]string) http.Header {
		headers := make(http.Header)
		headers.Set("anthropic-ratelimit-unified-status", "rejected")
		headers.Set("anthropic-ratelimit-unified-reset", strconv.FormatInt(now.Add(7*time.Hour).Unix(), 10))
		headers.Set("anthropic-ratelimit-unified-5h-utilization", "0.15")
		headers.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(now.Add(3*time.Hour).Unix(), 10))
		headers.Set("anthropic-ratelimit-unified-7d-utilization", "0.73")
		headers.Set("anthropic-ratelimit-unified-7d-reset", strconv.FormatInt(now.Add(5*time.Hour).Unix(), 10))
		if claim != "" {
			headers.Set("anthropic-ratelimit-unified-representative-claim", claim)
		}
		for name, value := range extra {
			headers.Set(name, value)
		}
		return headers
	}
	blockedUntil := func(model string) time.Duration {
		t.Helper()
		_, until := ClaudeUnavailableReason(monitor.accounts[claudeTestAccount.Key].fact, model, now)
		if until.IsZero() {
			return 0
		}
		return until.Sub(now).Round(time.Minute)
	}

	monitor.ObserveClaudeResponse(claudeTestAccount.Key, "claude-opus-5-5", rejection("", nil), http.StatusTooManyRequests)
	if got := blockedUntil("claude-opus-5-5"); got != time.Minute {
		t.Fatalf("throttle without a named limit held Opus for %s, want a minute", got)
	}
	if reason, _ := ClaudeUnavailableReason(monitor.accounts[claudeTestAccount.Key].fact, "claude-opus-5-5", now); reason != "Anthropic throttled it" {
		t.Fatalf("throttle reported as %q", reason)
	}
	if fact := monitor.accounts[claudeTestAccount.Key].fact; fact.AccountState != AccountReady {
		t.Fatalf("throttle on one model made the account %s", fact.AccountState)
	}
	if got := blockedUntil("claude-sonnet-5-5"); got != 0 {
		t.Fatalf("Opus throttle held Sonnet for %s", got)
	}

	monitor.ObserveClaudeResponse(claudeTestAccount.Key, "claude-opus-5-5", rejection("", map[string]string{"Retry-After": "600"}), http.StatusTooManyRequests)
	if got := blockedUntil("claude-opus-5-5"); got != 10*time.Minute {
		t.Fatalf("throttle held Opus for %s, want its 10 minute Retry-After", got)
	}

	monitor.ObserveClaudeResponse(claudeTestAccount.Key, "claude-opus-5-5", rejection("seven_day_opus", nil), http.StatusTooManyRequests)
	if got := blockedUntil("claude-opus-5-5"); got != 7*time.Hour {
		t.Fatalf("named Opus limit held Opus for %s, want until its reset in 7h", got)
	}
	if got := blockedUntil("claude-sonnet-5-5"); got != 0 {
		t.Fatalf("named Opus limit held Sonnet for %s", got)
	}

	monitor.ObserveClaudeResponse(claudeTestAccount.Key, "claude-opus-5-5", rejection("", map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": "1", "anthropic-ratelimit-unified-5h-status": "rejected"}), http.StatusTooManyRequests)
	if got := blockedUntil("claude-sonnet-5-5"); got != 3*time.Hour {
		t.Fatalf("five-hour limit held Sonnet for %s, want until its reset in 3h", got)
	}
}

// The Mac mini kept an Opus restriction saved by the earlier rule, from a
// throttle read as a usage limit. A restriction saved before restrictions
// named their limit is held only as long as a throttle, while one that names
// its limit keeps the account out until the reset.
func TestClaudeRestrictionSavedWithoutItsLimitIsHeldAsAThrottle(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "account-1"), 0700); err != nil {
		t.Fatal(err)
	}
	saved := `{"Restrictions":{"claude-opus-5-5":{"ResetsAt":"` + now.Add(7*time.Hour).Format(time.RFC3339) + `","ObservedAt":"` + now.Add(-2*time.Minute).Format(time.RFC3339) + `"},` +
		`"sonnet":{"ResetsAt":"` + now.Add(3*time.Hour).Format(time.RFC3339) + `","ObservedAt":"` + now.Add(-2*time.Minute).Format(time.RFC3339) + `","Claim":"seven_day_sonnet"}}}`
	if err := os.WriteFile(filepath.Join(data, "account-1", "claude-allowance.json"), []byte(saved), 0600); err != nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse("https://api.anthropic.com")
	monitor := NewMonitor(data, origin, "", http.DefaultClient, monitorTestDatabase(t))
	monitor.monitoredAccountLocked(claudeTestAccount)
	cache, _, err := monitor.reserveClaudeUsageRead(filepath.Join(data, "account-1"), now)
	if err != nil {
		t.Fatal(err)
	}
	fact := AccountRoutingFact{ClaudeRestrictions: cache.Restrictions}
	if reason, until := ClaudeUnavailableReason(fact, "claude-opus-5-5", now); reason != "" {
		t.Fatalf("restriction saved without its limit still holds Opus: %s until %s", reason, until.Sub(now))
	}
	if reason, until := ClaudeUnavailableReason(fact, "claude-sonnet-5-5", now); reason == "" || !until.Equal(now.Add(3*time.Hour)) {
		t.Fatalf("named Sonnet limit: %q until %s, want until its reset in 3h", reason, until.Sub(now))
	}
}

// A fast-mode request that names no usage limit is the proxy's to pass on; one
// that names a limit, in any of the forms the account rules read, is not.
func TestClaudeRejectionNamesUsageLimitAsTheAccountRulesDo(t *testing.T) {
	reset := strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10)
	rejection := func(fields map[string]string) http.Header {
		headers := make(http.Header)
		for name, value := range fields {
			headers.Set("anthropic-ratelimit-unified-"+name, value)
		}
		return headers
	}
	for _, check := range []struct {
		name   string
		fields map[string]string
		limit  bool
	}{
		{"credits refusal for fast mode, as on 31 October", map[string]string{"overage-disabled-reason": "org_level_disabled", "reset": reset}, false},
		{"throttle with an allowed window", map[string]string{"status": "rejected", "reset": reset, "5h-utilization": "0.15", "5h-status": "allowed", "5h-reset": reset}, false},
		{"five-hour window rejected", map[string]string{"5h-utilization": "0.4", "5h-status": "rejected", "5h-reset": reset}, true},
		{"five-hour window rate limited", map[string]string{"5h-utilization": "1.0", "5h-status": "rate_limited", "5h-reset": reset}, true},
		{"weekly window fully used", map[string]string{"7d-utilization": "1.0", "7d-reset": reset}, true},
		{"representative claim alone", map[string]string{"representative-claim": "seven_day_opus", "reset": reset}, true},
	} {
		if got := ClaudeRejectionNamesUsageLimit(rejection(check.fields)); got != check.limit {
			t.Errorf("%s: names a usage limit = %v, want %v", check.name, got, check.limit)
		}
	}
}

// Response headers carry no stored resets, so an ordinary Claude turn must not
// hide those the usage read reported.
func TestClaudeResetGrantsSurviveAnOrdinaryResponse(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	data := t.TempDir()
	if err := os.MkdirAll(filepath.Join(data, "account-1"), 0700); err != nil {
		t.Fatal(err)
	}
	origin, _ := url.Parse("https://api.anthropic.com")
	monitor := NewMonitor(data, origin, "", http.DefaultClient, monitorTestDatabase(t))
	monitor.monitoredAccountLocked(claudeTestAccount)
	grants := uint32(2)
	monitor.monitoredAccountLocked(claudeTestAccount).fact = AccountRoutingFact{Provider: ProviderClaude, AccountState: AccountReady, AllowanceSample: AllowanceSample{
		AccountID: "subscription", ObservedAt: now.Add(-time.Minute), AvailableResetCredits: &grants,
		StoredResets: []StoredReset{{Kind: ResetFable, Count: 2, ExpiresAt: now.Add(24 * time.Hour)}}, StoredResetsReadAt: now.Add(-time.Minute),
		Windows: []ReportedWindow{{ObservedAt: now.Add(-time.Minute), UsedPercent: 10, DurationSeconds: 5 * 3600, ResetsAt: now.Add(time.Hour)}},
	}}
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-5h-utilization", "0.2")
	headers.Set("anthropic-ratelimit-unified-5h-reset", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
	monitor.ObserveClaudeResponse(claudeTestAccount.Key, "claude-opus-4-5", headers, 200)
	fact := monitor.accounts[claudeTestAccount.Key].fact
	if count, list := HeldResets(fact, now); count == nil || *count != 2 || len(list) != 1 || list[0].Kind != ResetFable {
		t.Fatalf("stored resets after an ordinary response = %v %v, want 2 Fable resets", count, list)
	}
	// During an update a proxy built before the list may save its own reading
	// beside this one: a count of none, and no list.
	none := uint32(0)
	fromOlderBuild := AllowanceSample{ObservedAt: now.Add(time.Minute), AvailableResetCredits: &none}
	fact.AllowanceSample = mergeClaudeAllowance(fact.AllowanceSample, fromOlderBuild)
	if count, list := HeldResets(fact, now); count == nil || *count != 2 || len(list) != 1 {
		t.Fatalf("stored resets after an older build's reading = %v %v, want 2 Fable resets", count, list)
	}
}

// claudeLaunchGrants is Anthropic's list of an Ultra account's stored resets
// as it stood on 9 November 2026, the day it granted Fable resets: three
// grants of one reset each, which expire together.
const claudeLaunchGrants = `{"eligible":true,"ineligible_reason":null,"grants":[
 {"id":"launch-2610-fable","resets_total":1,"resets_left":1,"ends_at":"2027-03-09T07:59:00+00:00","clears":["seven_day_overage_included"],"paused":false,"usable_now":true,"type":"full","gives_back_weekly":true},
 {"id":"launch-2610-fable-12mo","resets_total":1,"resets_left":1,"ends_at":"2027-03-09T07:59:00+00:00","clears":["seven_day_overage_included"],"paused":false,"usable_now":true,"type":"full","gives_back_weekly":true},
 {"id":"launch-2610-fable-6mo","resets_total":1,"resets_left":1,"ends_at":"2027-03-09T07:59:00+00:00","clears":["seven_day_overage_included"],"paused":false,"usable_now":true,"type":"full","gives_back_weekly":true}],
 "next_grant_id":"launch-2610-fable"}`

// Anthropic lists an account's stored resets only to the Claude Code command
// line, and only to a release new enough. The proxy reads as the newest Claude
// Code that has sent it a request, so the list comes back when the person's
// Claude Code is new enough, whatever release this build was made with.
func TestClaudeStoredResetsAreReadAsTheNewestClaudeCodeServed(t *testing.T) {
	const minimum = "99.0.0"
	anthropic := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		product, _, _ := strings.Cut(request.Header.Get("User-Agent"), " ")
		version, isClaudeCode := strings.CutPrefix(product, "claude-cli/")
		grants := claudeLaunchGrants
		switch {
		case !isClaudeCode:
			grants = `{"eligible":false,"ineligible_reason":"surface","grants":[]}`
		case newerRelease(minimum, version):
			grants = `{"eligible":false,"ineligible_reason":"cli_version","grants":[]}`
		}
		fmt.Fprintf(writer, `{"five_hour":null,"cedar_ember":%s}`, grants)
	}))
	defer anthropic.Close()
	origin, _ := url.Parse(anthropic.URL)
	monitor := NewMonitor(t.TempDir(), origin, "", http.DefaultClient, monitorTestDatabase(t))
	monitor.SetClaudeUpstreamOrigin(origin)
	read := func() AllowanceSample {
		t.Helper()
		sample, _, err := monitor.readClaudeAllowance(context.Background(), claudeTestAccount, Credentials{AccessToken: "token"})
		if err != nil {
			t.Fatal(err)
		}
		return sample
	}

	if sample := read(); sample.NoResetsReason != "cli_version" || len(sample.StoredResets) != 0 {
		t.Fatalf("read as Claude Code %s: resets %v, reason %q; want none, for cli_version", builtInClaudeCodeVersion, sample.StoredResets, sample.NoResetsReason)
	}
	// Neither another client nor an older Claude Code changes what the proxy reads as.
	monitor.ObserveClaudeClient("pi/9.9.9")
	monitor.ObserveClaudeClient("claude-cli/2.1.100 (external, cli)")
	if sample := read(); sample.NoResetsReason != "cli_version" {
		t.Fatalf("after an older client, the proxy read as %s", monitor.claudeCodeVersion())
	}
	monitor.ObserveClaudeClient("claude-cli/99.1.0 (external, sdk-ts, agent-sdk/0.2.1)")
	sample := read()
	expires := time.Date(2027, 3, 9, 7, 59, 0, 0, time.UTC)
	if sample.NoResetsReason != "" || len(sample.StoredResets) != 1 || sample.StoredResets[0].Kind != ResetFable || sample.StoredResets[0].Count != 3 || !sample.StoredResets[0].ExpiresAt.Equal(expires) {
		t.Fatalf("read as Claude Code 99.1.0: resets %v, reason %q; want 3 Fable resets expiring %s", sample.StoredResets, sample.NoResetsReason, expires)
	}
	fact := AccountRoutingFact{Provider: ProviderClaude, AllowanceSample: sample}
	if got, want := describeStoredResets(fact, time.Date(2026, 11, 9, 21, 0, 0, 0, time.UTC)), "holds 3 Fable resets, expire 2027-03-09T07:59:00Z"; got != want {
		t.Fatalf("log describes the resets as %q, want %q", got, want)
	}
	// Once they have expired the account holds none, whatever the last reading counted.
	if count, list := HeldResets(fact, expires.Add(time.Minute)); count == nil || *count != 0 || len(list) != 0 {
		t.Fatalf("after the resets expired the account holds %v %v, want none", count, list)
	}
}

// OpenAI counts a Codex account's reset credits in every usage reading and
// lists them apart, less often. The list is given only while it agrees with
// the count: after a spend the count is right first.
func TestCodexStoredResetsAreListedWhileTheListAgreesWithTheCount(t *testing.T) {
	now := time.Now()
	credit := func(id string, expiresIn time.Duration) ResetCredit {
		return ResetCredit{ID: id, Status: "available", ResetType: "codex_rate_limits", ExpiresAt: now.Add(expiresIn)}
	}
	counted := func(count uint32) AccountRoutingFact {
		return AccountRoutingFact{AllowanceSample: AllowanceSample{AvailableResetCredits: &count}, ResetCreditsReadAt: now,
			ResetCredits: []ResetCredit{credit("later", 48*time.Hour), credit("sooner", 24*time.Hour), credit("expired", -time.Hour)}}
	}
	count, list := HeldResets(counted(2), now)
	if *count != 2 || len(list) != 2 || list[0].Kind != ResetWeekly || !list[0].ExpiresAt.Equal(now.Add(24*time.Hour)) {
		t.Fatalf("two credits counted and listed: got %d %v, want both, the sooner first", *count, list)
	}
	if count, list := HeldResets(counted(1), now); *count != 1 || list != nil {
		t.Fatalf("one credit counted, two listed: got %d %v, want the count alone", *count, list)
	}
}

// Fable's weekly window resets with the week's. Claude Code must be told the
// limit that holds the account: its "Fable limit" while other models still
// work, and its "weekly limit" only when the week itself is used up.
func TestClaudeLimitNamesTheWindowThatHoldsTheAccount(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	weekEnds := now.Add(40 * time.Hour)
	account := func(weekUsed, fableUsed float64) AccountRoutingFact {
		return AccountRoutingFact{Provider: ProviderClaude, AllowanceSample: AllowanceSample{Windows: []ReportedWindow{
			{DurationSeconds: 7 * 86400, UsedPercent: weekUsed, ResetsAt: weekEnds},
			{DurationSeconds: 7 * 86400, UsedPercent: fableUsed, ResetsAt: weekEnds, ModelName: "Fable"},
		}}}
	}
	for _, check := range []struct {
		name                string
		weekUsed, fableUsed float64
		claim               string
	}{
		{"Fable allowance used up", 60, 100, "seven_day_overage_included"},
		{"week used up with Fable allowance left", 100, 60, "seven_day"},
	} {
		availableAt, claim, found := ClaudeAvailableAgain(account(check.weekUsed, check.fableUsed), "claude-fable-5-2", now)
		if !found || !availableAt.Equal(weekEnds) || claim != check.claim {
			t.Errorf("%s: available again at %s as %q, want the week's reset as %q", check.name, availableAt.Sub(now), claim, check.claim)
		}
	}
	if reason, _ := ClaudeUnavailableReason(account(60, 100), "claude-opus-5-5", now); reason != "" {
		t.Errorf("a used-up Fable allowance keeps Opus off the account: %s", reason)
	}
}

// claudeTestAccount is the Claude account these tests observe, saved in account-1.
var claudeTestAccount = PoolAccount{Key: "claude:one@example.test", Folder: "account-1", Provider: ProviderClaude}
