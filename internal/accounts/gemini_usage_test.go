package accounts

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseAgyUsageJSON(t *testing.T) {
	raw := []byte(`{
		"conversation_id": "",
		"status": "SUCCESS",
		"duration_seconds": 0,
		"num_turns": 0,
		"command": {
			"name": "usage",
			"data": {
				"groups": [
					{
						"name": "Gemini Models",
						"buckets": [
							{"id": "gemini-weekly", "name": "Weekly Limit Remaining", "window": "weekly", "remaining_fraction": 0.7049, "reset_time": "2026-11-28T10:51:44Z"},
							{"id": "gemini-5h", "name": "Five Hour Limit Remaining", "window": "5h", "remaining_fraction": 0.9262, "reset_time": "2026-11-23T14:24:30Z"}
						]
					},
					{
						"name": "Claude and GPT models",
						"buckets": [
							{"id": "3p-weekly", "name": "Weekly Limit Remaining", "window": "weekly", "remaining_fraction": 0.9412, "reset_time": "2026-11-29T23:01:48Z"},
							{"id": "3p-5h", "name": "Five Hour Limit Remaining", "window": "5h", "remaining_fraction": 1, "reset_time": "2026-11-23T15:41:10Z"}
						]
					}
				]
			}
		}
	}`)
	observed := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	sample, err := parseAgyUsageJSON(raw, observed)
	if err != nil {
		t.Fatalf("parseAgyUsageJSON: %v", err)
	}
	if !sample.Allowed || sample.LimitReached {
		t.Fatalf("expected allowed sample, got allowed=%v limitReached=%v", sample.Allowed, sample.LimitReached)
	}
	if len(sample.Windows) != 4 {
		t.Fatalf("got %d windows, want 4", len(sample.Windows))
	}
	if sample.Windows[0].DurationSeconds != 5*3600 || sample.Windows[0].ModelID != "" {
		t.Fatalf("unexpected primary 5-hour window: %+v", sample.Windows[0])
	}
	if sample.Windows[1].DurationSeconds != 7*86400 || sample.Windows[1].ModelID != "" {
		t.Fatalf("unexpected primary weekly window: %+v", sample.Windows[1])
	}
	if diff := sample.Windows[1].UsedPercent - 29.51; diff < -0.01 || diff > 0.01 {
		t.Fatalf("weekly used percent = %.2f, want ~29.51", sample.Windows[1].UsedPercent)
	}
	if sample.Windows[2].ModelID != "3p" || sample.Windows[2].ModelName != "Claude & GPT" {
		t.Fatalf("unexpected 3p window identity: %+v", sample.Windows[2])
	}
}

func TestGeminiUsageMonitorDiscoversT3AndDeduplicates(t *testing.T) {
	home := t.TempDir()
	// Native login and two T3 provider dirs (one duplicate email, one distinct).
	writeToken := func(rel, email string) {
		t.Helper()
		full := filepath.Join(home, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(`{"id_token_email":"`+email+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeToken(".gemini/antigravity-cli/antigravity-oauth-token", "first@example.com")
	writeToken(".t3/userdata/providers/antigravity/aaa/antigravity-acp/antigravity-oauth-token", "FIRST@example.com")
	writeToken(".t3/userdata/providers/antigravity/bbb/antigravity-acp/antigravity-oauth-token", "second@example.com")

	// Fake agy CLI verifies isolation for T3 token symlink and returns valid JSON.
	fakeAgy := filepath.Join(t.TempDir(), "agy")
	script := `#!/bin/sh
if [ "$1" != "-p" ] || [ "$2" != "/usage" ] || [ "$3" != "--output-format" ] || [ "$4" != "json" ]; then
  echo "unexpected args: $*" >&2
  exit 2
fi
cat <<'EOF'
{"status":"SUCCESS","command":{"name":"usage","data":{"groups":[{"name":"Gemini Models","buckets":[{"id":"gemini-5h","window":"5h","remaining_fraction":0.8,"reset_time":"2026-11-23T18:00:00Z"}]}]}}}
EOF
`
	if err := os.WriteFile(fakeAgy, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	changed := false
	monitor := &GeminiUsageMonitor{
		homeDir:       home,
		agyExecutable: fakeAgy,
		interval:      time.Minute,
		now:           func() time.Time { return time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC) },
		onChange:      func() { changed = true },
	}
	monitor.refreshOnce(context.Background())

	if !changed {
		t.Fatal("expected onChange callback after first refresh")
	}
	accounts := monitor.ReadAccounts()
	if len(accounts) != 2 {
		t.Fatalf("got %d accounts, want 2 (deduplicated)", len(accounts))
	}
	if accounts[0].Key != "gemini:first@example.com" || accounts[1].Key != "gemini:second@example.com" {
		t.Fatalf("unexpected keys: %q, %q", accounts[0].Key, accounts[1].Key)
	}
	if accounts[0].UnavailableReason != "usage only" || len(accounts[0].AllowanceSample.Windows) != 1 {
		t.Fatalf("unexpected account[0]: %+v", accounts[0])
	}
}

func TestGeminiUsageMonitorAbortsImmediatelyOnAuthRequired(t *testing.T) {
	home := t.TempDir()
	tokenPath := filepath.Join(home, ".t3/userdata/providers/antigravity/aaa/antigravity-acp/antigravity-oauth-token")
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte(`{"id_token_email":"signedout@example.com"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Fake agy mimics real agy hanging 30s after printing Authentication required.
	fakeAgy := filepath.Join(t.TempDir(), "agy")
	script := `#!/bin/sh
echo "Authentication required. Please visit the URL to log in:" >&2
sleep 30
`
	if err := os.WriteFile(fakeAgy, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}

	monitor := &GeminiUsageMonitor{
		homeDir:       home,
		agyExecutable: fakeAgy,
		interval:      time.Minute,
		now:           time.Now,
	}
	start := time.Now()
	monitor.refreshOnce(context.Background())
	elapsed := time.Since(start)
	if elapsed > 3*time.Second {
		t.Fatalf("expected fast abort on Authentication required, took %s", elapsed)
	}
	accounts := monitor.ReadAccounts()
	if len(accounts) != 1 || accounts[0].State != AccountSignInRequired {
		t.Fatalf("expected AccountSignInRequired, got %+v", accounts)
	}
}

func TestFindAgyExecutableDisabled(t *testing.T) {
	t.Setenv("CODEX_SWITCH_AGY_BIN", "none")
	if got := findAgyExecutable(t.TempDir()); got != "" {
		t.Fatalf("findAgyExecutable = %q, want empty when disabled", got)
	}
}
