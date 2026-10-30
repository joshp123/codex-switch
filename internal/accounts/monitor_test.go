package accounts

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/joshp123/codex-switch/internal/store"
)

func TestAllowanceMonitoringFailurePreservesObservedExhaustion(t *testing.T) {
	data := t.TempDir()
	account := writeAccountFixture(t, filepath.Join(data, "account-1"), "observed-user", "observed@example.test", unexpiringAccessToken("observed-user"))
	status := http.StatusOK
	limited := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/backend-api/codex/models" {
			_, _ = w.Write([]byte(`{"models":[{"slug":"astra"}]}`))
			return
		}
		w.WriteHeader(status)
		if limited {
			_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"allowed":false,"limit_reached":true}}`))
		} else {
			_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false}}`))
		}
	}))
	defer upstream.Close()
	origin, _ := url.Parse(upstream.URL)
	monitor := NewMonitor(data, origin, "", upstream.Client(), monitorTestDatabase(t))
	first := readAccountNow(t, monitor, account)
	status = http.StatusServiceUnavailable
	failed := readAccountNow(t, monitor, account)
	if failed.AccountState != AccountLimited || !failed.RefreshedAt.Equal(first.RefreshedAt) || !failed.AllowanceSample.LimitReached {
		t.Fatalf("monitor failure erased exhaustion: state=%s observation=%v", failed.AccountState, failed.RefreshedAt)
	}
	status, limited = http.StatusOK, false
	restored := readAccountNow(t, monitor, account)
	if restored.AccountState != AccountReady || !restored.RefreshedAt.After(first.RefreshedAt) {
		t.Fatalf("new observation did not restore capacity: %s", restored.AccountState)
	}
}

func TestAccountRefreshDoesNotBlockOtherAccountsOrCachedReads(t *testing.T) {
	data := t.TempDir()
	for _, name := range []string{"account-1", "account-2"} {
		writeAccountFixture(t, filepath.Join(data, name), name, name+"@example.test", unexpiringAccessToken(name))
	}
	delayed := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("ChatGPT-Account-ID") == "subscription-account-1" {
			select {
			case delayed <- struct{}{}:
			default:
			}
			<-r.Context().Done()
			return
		}
		if r.URL.Path == "/backend-api/codex/models" {
			_, _ = w.Write([]byte(`{"models":[{"slug":"astra"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false}}`))
	}))
	defer upstream.Close()
	origin, _ := url.Parse(upstream.URL)
	monitor := NewMonitor(data, origin, "", upstream.Client(), monitorTestDatabase(t))
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() { defer close(stopped); monitor.Run(ctx) }()
	defer func() { cancel(); <-stopped }()
	select {
	case <-delayed:
	case <-time.After(time.Second):
		t.Fatal("delayed account was not read")
	}
	deadline := time.After(2 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		facts, err := monitor.ReadAccountRoutingFacts()
		if err != nil {
			t.Fatal(err)
		}
		for _, fact := range facts {
			if fact.AccountName == "account-2" && fact.AccountState == AccountReady {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatal("healthy account waited for delayed account")
		case <-ticker.C:
		}
	}
}

func TestAllowanceIdentityRequiresPositiveUsageAndExplicitReset(t *testing.T) {
	for _, test := range []struct {
		name        string
		usedPercent int
		resetFields string
		wantOwner   bool
	}{
		{"explicit used window", 54, `"reset_at":1789163887`, true},
		{"unused window", 0, `"reset_at":1789163887`, false},
		{"countdown only", 54, `"reset_after_seconds":300`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprintf(w, `{"rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":%d,"limit_window_seconds":604800,%s}}}`, test.usedPercent, test.resetFields)
			}))
			defer server.Close()
			database := monitorTestDatabase(t)
			origin, _ := url.Parse(server.URL)
			monitor := NewMonitor(t.TempDir(), origin, "", server.Client(), database)
			sample, err := monitor.readAllowanceSample(t.Context(), PoolAccount{Name: "account-1"}, Credentials{AccountID: "subscription-1"})
			if err != nil {
				t.Fatal(err)
			}
			window := sample.Windows[0]
			owner, found, err := database.FindAccountForAllowanceWindow(window.DurationSeconds, window.ResetsAt, window.ResetsAt.Add(-time.Second))
			if err != nil || found != test.wantOwner || (found && owner != "subscription-1") {
				t.Fatalf("allowance owner = %q, found = %v, error = %v", owner, found, err)
			}
		})
	}
}

type accountRevocationTransport func(*http.Request) (*http.Response, error)

func (f accountRevocationTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// A revoked login stays signed out, through allowance outages, until the
// person signs in again; routing then has the account back.
func TestRevokedLoginIsUnavailableUntilSignInRepairsIt(t *testing.T) {
	for _, rejectionSource := range []string{"allowance observation", "model request", "model request during allowance observation"} {
		t.Run(rejectionSource, func(t *testing.T) {
			data := t.TempDir()
			account := writeAccountFixture(t, filepath.Join(data, "account-1"), "revoked-user", "revoked@example.test", unexpiringAccessToken("revoked-user"))
			revoked := false
			allowanceUnavailable := false
			holdAllowance := false
			allowanceStarted := make(chan struct{})
			releaseAllowance := make(chan struct{})
			allowanceReleased := make(chan struct{})
			repairedAccessToken := unexpiringAccessToken("repaired-user")
			client := &http.Client{Transport: accountRevocationTransport(func(r *http.Request) (*http.Response, error) {
				status, body := 200, `{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false}}`
				if holdAllowance && r.URL.Path == "/backend-api/wham/usage" {
					close(allowanceStarted)
					select {
					case <-releaseAllowance:
						close(allowanceReleased)
						return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: io.NopCloser(strings.NewReader("")), Header: make(http.Header), Request: r}, nil
					case <-r.Context().Done():
						return nil, r.Context().Err()
					}
				}
				if r.URL.Path == "/backend-api/codex/models" {
					body = `{"models":[{"slug":"astra"}]}`
				}
				if revoked && r.Header.Get("Authorization") != "Bearer "+repairedAccessToken {
					status = 401
					body = `{"error":{"code":"refresh_token_reused"}}`
				}
				if allowanceUnavailable && r.URL.Path == "/backend-api/wham/usage" {
					status = http.StatusServiceUnavailable
				}
				return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header), Request: r}, nil
			})}
			origin, _ := url.Parse("http://isolated-lifecycle.invalid")
			monitor := NewMonitor(data, origin, "", client, monitorTestDatabase(t))
			if before := readAccountNow(t, monitor, account); before.AccountState != AccountReady {
				t.Fatalf("operating account was %s before revocation", before.AccountState)
			}
			rejectedCredentials := currentFact(t, monitor).Credentials
			revoked = true
			switch rejectionSource {
			case "model request during allowance observation":
				holdAllowance = true
				read := make(chan struct{})
				go func() { defer close(read); runScheduledRead(monitor, account) }()
				select {
				case <-allowanceStarted:
				case <-time.After(time.Second):
					t.Fatal("background allowance observation did not start")
				}
				_ = monitor.RefreshAccountCredentialsAfterUnauthorized("account-1", rejectedCredentials)
				close(releaseAllowance)
				<-allowanceReleased
				<-read
				holdAllowance = false
			case "model request":
				_ = monitor.RefreshAccountCredentialsAfterUnauthorized("account-1", rejectedCredentials)
			default:
				readAccountNow(t, monitor, account)
			}
			if after := currentFact(t, monitor); after.AccountState != AccountSignInRequired {
				t.Fatalf("revoked login state is %s", after.AccountState)
			}
			allowanceUnavailable = true
			if afterOutage := readAccountNow(t, monitor, account); afterOutage.AccountState != AccountSignInRequired {
				t.Fatalf("allowance outage revived the rejected login: %s", afterOutage.AccountState)
			}
			staged := filepath.Join(t.TempDir(), "login")
			repaired := writeAccountFixture(t, staged, "revoked-user", "revoked@example.test", repairedAccessToken)
			if _, err := publishLogin(data, staged, account.Name, repaired); err != nil {
				t.Fatal(err)
			}
			allowanceUnavailable = false
			if restored := readAccountNow(t, monitor, account); restored.AccountState != AccountReady {
				t.Fatalf("repaired login remains %s", restored.AccountState)
			}
		})
	}
}

// readAccountNow runs one scheduled read of account, as Run does, and returns
// the resulting fact.
func readAccountNow(t *testing.T, monitor *Monitor, account PoolAccount) AccountRoutingFact {
	t.Helper()
	runScheduledRead(monitor, account)
	return currentFact(t, monitor)
}

func runScheduledRead(monitor *Monitor, account PoolAccount) {
	monitor.mutex.Lock()
	monitored := monitor.monitoredAccountLocked(store.AccountName(account.Name))
	monitored.reading = true
	scheduled := monitored.fact
	monitor.mutex.Unlock()
	monitor.readAccount(context.Background(), account, monitored, scheduled, true)
}

func currentFact(t *testing.T, monitor *Monitor) AccountRoutingFact {
	t.Helper()
	facts, err := monitor.ReadAccountRoutingFacts()
	if err != nil || len(facts) != 1 {
		t.Fatalf("facts = %v, error = %v", facts, err)
	}
	return facts[0]
}

func monitorTestDatabase(t *testing.T) *store.StoreDatabase {
	t.Helper()
	database, err := store.OpenStoreDatabase(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.CloseStoreDatabase() })
	return database
}

func writeAccountFixture(t *testing.T, home, userID, email, accessToken string) PoolAccount {
	t.Helper()
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	header, _ := json.Marshal(map[string]string{"alg": "none"})
	payload, _ := json.Marshal(map[string]any{
		"email":                       email,
		"https://api.openai.com/auth": map[string]string{"chatgpt_user_id": userID},
	})
	idToken := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".x"
	data, _ := json.Marshal(map[string]any{
		"auth_mode": "chatgpt",
		"tokens": map[string]string{
			"account_id": "subscription-" + userID, "id_token": idToken,
			"access_token": accessToken, "refresh_token": "refresh-" + userID,
		},
	})
	if err := os.WriteFile(filepath.Join(home, "auth.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	account, err := ReadPoolAccount(home, filepath.Base(home), 0)
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func unexpiringAccessToken(subject string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := []byte(fmt.Sprintf(`{"exp":4102444800,"sub":%q}`, subject))
	return header + "." + base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func TestAccountMonitorWakesAtNextReadDeadline(t *testing.T) {
	data := t.TempDir()
	account := writeAccountFixture(t, filepath.Join(data, "account-1"), "deadline-user", "deadline@example.test", unexpiringAccessToken("deadline-user"))
	reads := make(chan struct{}, 10)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/backend-api/codex/models" {
			_, _ = w.Write([]byte(`{"models":[{"slug":"astra"}]}`))
			return
		}
		reads <- struct{}{}
		_, _ = w.Write([]byte(`{"plan_type":"pro","rate_limit":{"allowed":true,"limit_reached":false}}`))
	}))
	defer upstream.Close()
	origin, _ := url.Parse(upstream.URL)
	monitor := NewMonitor(data, origin, "", upstream.Client(), monitorTestDatabase(t))
	monitored := monitor.monitoredAccountLocked(store.AccountName(account.Name))
	monitored.lastReadStarted = time.Now().Add(-accountReadInterval + 100*time.Millisecond)
	expectedDue := monitored.lastReadStarted.Add(accountReadInterval)
	var pending sync.WaitGroup
	if got := monitor.startDueReads(t.Context(), &pending, false); !got.Equal(expectedDue) {
		t.Fatalf("next read %s, want %s", got, expectedDue)
	}
	select {
	case <-reads:
		t.Fatal("account was read before due")
	default:
	}
	ctx, cancel := context.WithCancel(t.Context())
	stopped := make(chan struct{})
	go func() { defer close(stopped); monitor.Run(ctx) }()
	defer func() { cancel(); <-stopped }()
	select {
	case <-reads:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor did not wake at the account deadline")
	}
	deadline := time.Now().Add(time.Second)
	for {
		monitor.mutex.Lock()
		finished := !monitored.reading
		if finished {
			monitored.lastReadStarted = time.Now().Add(-requestedAccountReadInterval - time.Second)
		}
		monitor.mutex.Unlock()
		if finished {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scheduled read did not finish")
		}
		time.Sleep(time.Millisecond)
	}
	// A routing request wakes the monitor even though its regular read is not due.
	monitor.RequestAccountRoutingFactsRefresh()
	select {
	case <-reads:
	case <-time.After(2 * time.Second):
		t.Fatal("routing request did not wake the monitor")
	}
}

func TestAccountMonitorWaitsWhileOverdueReadIsInFlight(t *testing.T) {
	data := t.TempDir()
	account := writeAccountFixture(t, filepath.Join(data, "account-1"), "inflight-user", "inflight@example.test", unexpiringAccessToken("inflight-user"))
	origin, _ := url.Parse("http://127.0.0.1")
	monitor := NewMonitor(data, origin, "", http.DefaultClient, monitorTestDatabase(t))
	monitored := monitor.monitoredAccountLocked(store.AccountName(account.Name))
	monitored.reading = true
	monitored.lastReadStarted = time.Now().Add(-2 * accountReadInterval)
	before := time.Now()
	var pending sync.WaitGroup
	nextRead := monitor.startDueReads(t.Context(), &pending, false)
	after := time.Now()
	if nextRead.Before(before.Add(accountReadInterval)) || nextRead.After(after.Add(accountReadInterval)) {
		t.Fatalf("in-flight read scheduled an overdue timer: next read %s, now %s", nextRead, after)
	}
}

func TestProviderCheckPassesThrottledReadsAndFailsOutages(t *testing.T) {
	for _, test := range []struct {
		status int
		passes bool
	}{
		{http.StatusTooManyRequests, true},
		{http.StatusServiceUnavailable, false},
	} {
		data := t.TempDir()
		writeAccountFixture(t, filepath.Join(data, "account-1"), "account-1", "account-1@example.test", unexpiringAccessToken("account-1"))
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(test.status) }))
		defer upstream.Close()
		origin, _ := url.Parse(upstream.URL)
		monitor := NewMonitor(data, origin, "", upstream.Client(), monitorTestDatabase(t))
		if err := monitor.CheckProviderAccess(t.Context()); (err == nil) != test.passes {
			t.Fatalf("usage status %d: check returned %v", test.status, err)
		}
	}
}
