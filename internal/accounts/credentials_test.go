package accounts

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestRefreshRotatesAndPersistsTokens(t *testing.T) {
	home := t.TempDir()
	writeTestAuth(t, home, jwt(t, time.Now().Add(-time.Minute), "account"), "old-refresh", "account")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Content-Type") != "application/json" {
			t.Fatalf("content type = %q", r.Header.Get("Content-Type"))
		}
		var request map[string]string
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request["grant_type"] != "refresh_token" || request["refresh_token"] != "old-refresh" || request["client_id"] != clientID {
			t.Fatalf("request = %v", request)
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  jwt(t, time.Now().Add(time.Hour), "account"),
			"refresh_token": "new-refresh",
			"id_token":      jwt(t, time.Now().Add(time.Hour), "account"),
		})
	}))
	defer server.Close()
	store := newSavedLogin(home, server.Client())
	store.token = server.URL
	credentials, err := store.Credentials(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if credentials.AccountID != "account" {
		t.Fatalf("account = %q", credentials.AccountID)
	}
	data, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted authFile
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Tokens.RefreshToken != "new-refresh" {
		t.Fatalf("refresh token was not rotated")
	}
}

func TestUnauthorizedRefreshRotatesARejectedTokenOnlyOnce(t *testing.T) {
	home := t.TempDir()
	rejected := jwt(t, time.Now().Add(time.Hour), "account")
	replacement := jwt(t, time.Now().Add(2*time.Hour), "account")
	writeTestAuth(t, home, rejected, "old-refresh", "account")
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		refreshes.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token": replacement, "refresh_token": "new-refresh",
		})
	}))
	defer server.Close()
	store := newSavedLogin(home, server.Client())
	store.token = server.URL

	var wait sync.WaitGroup
	results := make(chan Credentials, 2)
	errors := make(chan error, 2)
	for range 2 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			credentials, err := store.RefreshAfterUnauthorized(t.Context(), Credentials{
				AccessToken: rejected, AccountID: "account",
			})
			results <- credentials
			errors <- err
		}()
	}
	wait.Wait()
	close(results)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	for credentials := range results {
		if credentials.AccessToken != replacement || credentials.AccountID != "account" {
			t.Fatalf("credentials = %#v", credentials)
		}
	}
	if refreshes.Load() != 1 {
		t.Fatalf("refresh requests = %d, want 1", refreshes.Load())
	}
}

func TestUnauthorizedRefreshRejectsAStoredReplacementFromAnotherAccount(t *testing.T) {
	home := t.TempDir()
	rejected := Credentials{
		AccessToken: jwt(t, time.Now().Add(time.Hour), "account-a"),
		AccountID:   "account-a",
	}
	writeTestAuth(t, home, rejected.AccessToken, "refresh-a", rejected.AccountID)
	var refreshes atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		refreshes.Add(1)
	}))
	defer server.Close()
	store := newSavedLogin(home, server.Client())
	store.token = server.URL

	replacement := jwt(t, time.Now().Add(2*time.Hour), "account-b")
	writeTestAuth(t, home, replacement, "refresh-b", "account-b")
	got, err := store.RefreshAfterUnauthorized(t.Context(), rejected)
	if !requiresSignIn(err) || got != (Credentials{}) {
		t.Fatalf("replacement credentials = %#v, error = %T %v", got, err, err)
	}
	if refreshes.Load() != 0 {
		t.Fatalf("cross-account replacement triggered %d refreshes", refreshes.Load())
	}
	persisted, err := store.read()
	if err != nil {
		t.Fatal(err)
	}
	if persisted.Tokens.AccountID != "account-b" || persisted.Tokens.AccessToken != replacement ||
		persisted.Tokens.RefreshToken != "refresh-b" {
		t.Fatalf("replacement account was mutated: %#v", persisted.Tokens)
	}
}

func TestRefreshRejectsAnotherAccountWithoutChangingStoredCredentials(t *testing.T) {
	home := t.TempDir()
	originalAccess := jwt(t, time.Now().Add(-time.Minute), "account")
	writeTestAuth(t, home, originalAccess, "old-refresh", "account")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  jwt(t, time.Now().Add(time.Hour), "other-account"),
			"refresh_token": "other-refresh",
			"id_token":      jwt(t, time.Now().Add(time.Hour), "other-account"),
		})
	}))
	defer server.Close()
	store := newSavedLogin(home, server.Client())
	store.token = server.URL

	if _, err := store.Credentials(t.Context()); !requiresSignIn(err) {
		t.Fatalf("refresh error = %T %v", err, err)
	}
	data, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted authFile
	if err := json.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Tokens.AccessToken != originalAccess || persisted.Tokens.RefreshToken != "old-refresh" || persisted.Tokens.AccountID != "account" {
		t.Fatalf("stored credentials changed: %#v", persisted.Tokens)
	}
}

func TestRefreshRejectsTrailingOrOversizedSuccessWithoutChangingAuth(t *testing.T) {
	oversized := `{"access_token":"` + strings.Repeat("x", maximumRefreshResponseBytes) + `"}`
	tests := []struct {
		name string
		body string
	}{
		{name: "second document", body: `{"access_token":"new"} {}`},
		{name: "trailing garbage", body: `{"access_token":"new"} private provider response`},
		{name: "oversized", body: oversized},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			writeTestAuth(t, home, jwt(t, time.Now().Add(-time.Minute), "account"), "refresh", "account")
			path := filepath.Join(home, "auth.json")
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			store := newSavedLogin(home, server.Client())
			store.token = server.URL

			if _, err := store.Credentials(t.Context()); err == nil || requiresSignIn(err) ||
				strings.Contains(err.Error(), "private provider response") {
				t.Fatalf("refresh error = %T %v", err, err)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(after, before) {
				t.Fatal("failed refresh changed stored auth")
			}
		})
	}
}

func TestRefresh401RequiresSignInWithoutExposingTheResponse(t *testing.T) {
	for _, body := range []string{
		`{"error":{"code":"refresh_token_expired","message":"secret detail"}}`,
		`{"error":"refresh_token_reused","detail":"secret detail"}`,
		`{"error":{"code":"refresh_token_invalidated"}}`,
		`{"unknown":"secret detail"}`,
	} {
		t.Run(body, func(t *testing.T) {
			home := t.TempDir()
			writeTestAuth(t, home, jwt(t, time.Now().Add(-time.Minute), "account"), "refresh", "account")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(body))
			}))
			defer server.Close()
			store := newSavedLogin(home, server.Client())
			store.token = server.URL
			_, err := store.Credentials(t.Context())
			if !requiresSignIn(err) {
				t.Fatalf("error = %T %v", err, err)
			}
			if strings.Contains(err.Error(), "secret detail") {
				t.Fatalf("error exposed response content: %v", err)
			}
		})
	}
}

func TestRefreshServerFailureIsTemporary(t *testing.T) {
	home := t.TempDir()
	writeTestAuth(t, home, jwt(t, time.Now().Add(-time.Minute), "account"), "refresh", "account")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "secret detail", http.StatusBadGateway)
	}))
	defer server.Close()
	store := newSavedLogin(home, server.Client())
	store.token = server.URL
	_, err := store.Credentials(t.Context())
	if err == nil || requiresSignIn(err) {
		t.Fatalf("error = %T %v", err, err)
	}
	if strings.Contains(err.Error(), "secret detail") {
		t.Fatalf("error exposed response content: %v", err)
	}
}

func TestRefreshHoldsThePerAccountLockUntilItsAtomicWriteFinishes(t *testing.T) {
	home := t.TempDir()
	writeTestAuth(t, home, jwt(t, time.Now().Add(-time.Minute), "account"), "old-refresh", "account")
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	newAccess := jwt(t, time.Now().Add(time.Hour), "account")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(refreshStarted)
		<-releaseRefresh
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  newAccess,
			"refresh_token": "rotated-refresh",
		})
	}))
	defer server.Close()
	store := newSavedLogin(home, server.Client())
	store.token = server.URL
	credentialsDone := make(chan error, 1)
	go func() {
		_, err := store.Credentials(t.Context())
		credentialsDone <- err
	}()
	<-refreshStarted

	lockAcquired := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- withAuthLock(home, func() error {
			close(lockAcquired)
			return nil
		})
	}()
	select {
	case <-lockAcquired:
		t.Fatal("relogin lock was acquired while refresh was still writing")
	case <-time.After(20 * time.Millisecond):
	}
	close(releaseRefresh)
	if err := <-credentialsDone; err != nil {
		t.Fatal(err)
	}
	select {
	case <-lockAcquired:
	case <-time.After(time.Second):
		t.Fatal("relogin lock was not released after refresh")
	}
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
}

func TestMissingMalformedAndIncompleteCredentialsRequireSignIn(t *testing.T) {
	for _, test := range []struct {
		name string
		data []byte
	}{
		{name: "missing"},
		{name: "malformed", data: []byte(`{`)},
		{name: "incomplete", data: []byte(`{"auth_mode":"chatgpt","tokens":{}}`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			if test.data != nil {
				if err := os.WriteFile(filepath.Join(home, "auth.json"), test.data, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			_, err := newSavedLogin(home, nil).Credentials(t.Context())
			if !requiresSignIn(err) {
				t.Fatalf("error = %T %v", err, err)
			}
		})
	}
}

func writeTestAuth(t *testing.T, home, access, refresh, account string) {
	t.Helper()
	value := authFile{AuthMode: "chatgpt", LastRefresh: time.Now(), Tokens: tokens{AccessToken: access, RefreshToken: refresh, IDToken: access, AccountID: account}}
	data, _ := json.Marshal(value)
	if err := os.WriteFile(filepath.Join(home, "auth.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func jwt(t *testing.T, expiry time.Time, account string) string {
	t.Helper()
	header, _ := json.Marshal(map[string]string{"alg": "none"})
	payload, _ := json.Marshal(map[string]any{"exp": expiry.Unix(), "chatgpt_account_id": account})
	return base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload) + ".x"
}

func TestRefreshInvalidGrantRequiresSignInAndOtherBadRequestsRemainTemporary(t *testing.T) {
	for _, test := range []struct {
		name, body  string
		status      int
		needsSignIn bool
	}{
		{"invalid grant", `{"error":"invalid_grant","error_description":"secret detail"}`, http.StatusBadRequest, true},
		{"bad client request", `{"error":"invalid_request","error_description":"secret detail"}`, http.StatusBadRequest, false},
		{"provider outage", `{"error":"invalid_grant","error_description":"secret detail"}`, http.StatusBadGateway, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			home := t.TempDir()
			writeTestAuth(t, home, jwt(t, time.Now().Add(-time.Minute), "account"), "refresh", "account")
			before, err := os.ReadFile(filepath.Join(home, "auth.json"))
			if err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(test.status)
				io.WriteString(writer, test.body)
			}))
			defer server.Close()
			login := newSavedLogin(home, server.Client())
			login.token = server.URL
			_, err = login.Credentials(t.Context())
			if err == nil || requiresSignIn(err) != test.needsSignIn {
				t.Fatalf("refresh error = %v; needs sign in %t", err, test.needsSignIn)
			}
			if strings.Contains(err.Error(), "secret detail") {
				t.Fatal("refresh error exposed response content")
			}
			after, err := os.ReadFile(filepath.Join(home, "auth.json"))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("rejected refresh changed stored credentials")
			}
		})
	}
}
