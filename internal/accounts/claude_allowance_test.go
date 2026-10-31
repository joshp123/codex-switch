package accounts

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
	var payload claudeUsageResponse
	if err := json.Unmarshal([]byte(`{"limits":[{"kind":"weekly_all","percent":14,"resets_at":"`+now.Add(7*24*time.Hour).Format(time.RFC3339)+`"},{"kind":"weekly_scoped","percent":100,"resets_at":"`+now.Add(7*24*time.Hour).Format(time.RFC3339)+`","scope":{"model":{"id":null,"display_name":"Fable"}}}]}`), &payload); err != nil {
		t.Fatal(err)
	}
	sample, err := payload.sample(PoolAccount{AccountID: "subscription", Plan: "Max 20x"}, now)
	if err != nil {
		t.Fatal(err)
	}
	monitor.monitoredAccountLocked("account-1", ProviderClaude).fact = AccountRoutingFact{Provider: ProviderClaude, AccountState: AccountReady, AllowanceSample: sample}
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-7d-utilization", "0.15")
	headers.Set("anthropic-ratelimit-unified-7d-reset", strconv.FormatInt(now.Add(7*24*time.Hour).Unix(), 10))
	monitor.ObserveClaudeResponse("account-1", "claude-haiku-4-5", headers, 200)
	fact := monitor.accounts["account-1"].fact
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
	cache, _, err := restarted.reserveClaudeUsageRead("account-1", now)
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
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-7d_oi-utilization", "1")
	headers.Set("anthropic-ratelimit-unified-7d_oi-reset", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
	headers.Set("anthropic-ratelimit-unified-7d_oi-status", "rejected")
	monitor.ObserveClaudeResponse("account-1", "claude-fable-5", headers, 429)
	fact := monitor.accounts["account-1"].fact
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
	monitor.ObserveClaudeResponse("account-1", "claude-haiku-4-5", sonnetHeaders, 200)
	fact = monitor.accounts["account-1"].fact
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
	fact := AccountRoutingFact{AllowanceSample: AllowanceSample{Windows: []ReportedWindow{{ModelID: "claude-opus-4-5-20251101", ModelName: "Opus", DurationSeconds: 7 * 86400, UsedPercent: 100, ResetsAt: now.Add(time.Hour)}}}}
	monitor.monitoredAccountLocked("account-1", ProviderClaude).fact = fact
	headers := make(http.Header)
	headers.Set("anthropic-ratelimit-unified-7d_oi-utilization", "1")
	headers.Set("anthropic-ratelimit-unified-7d_oi-status", "rejected")
	headers.Set("anthropic-ratelimit-unified-7d_oi-reset", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
	monitor.ObserveClaudeResponse("account-1", "claude-opus-4-5", headers, 429)
	fact = monitor.accounts["account-1"].fact
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
	cache, due, err := first.reserveClaudeUsageRead("account-1", now)
	if err != nil || !due {
		t.Fatalf("first read: due=%v err=%v", due, err)
	}
	cache.ReadAfter = now.Add(time.Hour)
	if err := first.saveClaudeAllowanceCache("account-1", cache); err != nil {
		t.Fatal(err)
	}
	if _, due, err := second.reserveClaudeUsageRead("account-1", now.Add(30*time.Minute)); err != nil || due {
		t.Fatalf("backoff was ignored: due=%v err=%v", due, err)
	}
	if _, due, err := second.reserveClaudeUsageRead("account-1", now.Add(time.Hour)); err != nil || !due {
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
		_, until := ClaudeUnavailableReason(monitor.accounts["account-1"].fact, model, now)
		if until.IsZero() {
			return 0
		}
		return until.Sub(now).Round(time.Minute)
	}

	monitor.ObserveClaudeResponse("account-1", "claude-opus-5-5", rejection("", nil), http.StatusTooManyRequests)
	if got := blockedUntil("claude-opus-5-5"); got != time.Minute {
		t.Fatalf("throttle without a named limit held Opus for %s, want a minute", got)
	}
	if reason, _ := ClaudeUnavailableReason(monitor.accounts["account-1"].fact, "claude-opus-5-5", now); reason != "Anthropic throttled it" {
		t.Fatalf("throttle reported as %q", reason)
	}
	if fact := monitor.accounts["account-1"].fact; fact.AccountState != AccountReady {
		t.Fatalf("throttle on one model made the account %s", fact.AccountState)
	}
	if got := blockedUntil("claude-sonnet-5-5"); got != 0 {
		t.Fatalf("Opus throttle held Sonnet for %s", got)
	}

	monitor.ObserveClaudeResponse("account-1", "claude-opus-5-5", rejection("", map[string]string{"Retry-After": "600"}), http.StatusTooManyRequests)
	if got := blockedUntil("claude-opus-5-5"); got != 10*time.Minute {
		t.Fatalf("throttle held Opus for %s, want its 10 minute Retry-After", got)
	}

	monitor.ObserveClaudeResponse("account-1", "claude-opus-5-5", rejection("seven_day_opus", nil), http.StatusTooManyRequests)
	if got := blockedUntil("claude-opus-5-5"); got != 7*time.Hour {
		t.Fatalf("named Opus limit held Opus for %s, want until its reset in 7h", got)
	}
	if got := blockedUntil("claude-sonnet-5-5"); got != 0 {
		t.Fatalf("named Opus limit held Sonnet for %s", got)
	}

	monitor.ObserveClaudeResponse("account-1", "claude-opus-5-5", rejection("", map[string]string{
		"anthropic-ratelimit-unified-5h-utilization": "1", "anthropic-ratelimit-unified-5h-status": "rejected"}), http.StatusTooManyRequests)
	if got := blockedUntil("claude-sonnet-5-5"); got != 3*time.Hour {
		t.Fatalf("five-hour limit held Sonnet for %s, want until its reset in 3h", got)
	}
}
