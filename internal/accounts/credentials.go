package accounts

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	clientID                    = "app_EMoamEEZ73f0CkXaXp7hrann"
	tokenURL                    = "https://auth.openai.com/oauth/token"
	maximumRefreshResponseBytes = 1 << 20
)

type credentialErrorKind string

const (
	credentialErrorTemporary      credentialErrorKind = "temporary"
	credentialErrorSignInRequired credentialErrorKind = "sign-in-required"
)

type credentialError struct {
	Kind  credentialErrorKind
	Cause error
}

func (e *credentialError) Error() string {
	if e.Kind == credentialErrorSignInRequired {
		return "sign-in required"
	}
	if e.Cause != nil {
		return e.Cause.Error()
	}
	return "credential check failed"
}

func (e *credentialError) Unwrap() error {
	return e.Cause
}

func requiresSignIn(err error) bool {
	var credentialError *credentialError
	return errors.As(err, &credentialError) && credentialError.Kind == credentialErrorSignInRequired
}

func signInRequired(cause error) error {
	return &credentialError{Kind: credentialErrorSignInRequired, Cause: cause}
}

func temporary(cause error) error {
	return &credentialError{Kind: credentialErrorTemporary, Cause: cause}
}

type Credentials struct {
	AccessToken string
	AccountID   string
}

// savedLogin reads one account's auth.json and refreshes its tokens.
type savedLogin struct {
	provider Provider
	mu       sync.Mutex
	home     string
	client   *http.Client
	token    string
}

type authFile struct {
	APIKey      *string   `json:"OPENAI_API_KEY"`
	AuthMode    string    `json:"auth_mode"`
	LastRefresh time.Time `json:"last_refresh"`
	Tokens      tokens    `json:"tokens"`
}

type tokens struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	AccountID    string `json:"account_id"`
}

func newSavedLogin(home string, client *http.Client) *savedLogin {
	if client == nil {
		client = http.DefaultClient
	}
	return &savedLogin{home: home, client: client, token: tokenURL}
}

func (s *savedLogin) Credentials(ctx context.Context) (Credentials, error) {
	if s.provider == ProviderClaude {
		return s.claudeCredentials(ctx, Credentials{})
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var result Credentials
	err := withAuthLock(s.home, func() error {
		auth, err := s.read()
		if err != nil {
			return err
		}
		if expiresSoon(auth.Tokens.AccessToken, 5*time.Minute) {
			if err := s.refresh(ctx, &auth); err != nil {
				return err
			}
		}
		result = Credentials{AccessToken: auth.Tokens.AccessToken, AccountID: auth.Tokens.AccountID}
		return nil
	})
	return result, err
}

// CurrentCredentials reads the stored ChatGPT credentials without refreshing
// or modifying them. It accepts only a current access token.
func (s *savedLogin) CurrentCredentials() (Credentials, error) {
	if s.provider == ProviderClaude {
		return s.currentClaudeCredentials()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	auth, err := s.read()
	if err != nil {
		return Credentials{}, err
	}
	if expiresSoon(auth.Tokens.AccessToken, 5*time.Minute) {
		return Credentials{}, signInRequired(errors.New("stored access token is expired or invalid"))
	}
	return Credentials{AccessToken: auth.Tokens.AccessToken, AccountID: auth.Tokens.AccountID}, nil
}

// RefreshAfterUnauthorized refreshes only when rejected is still stored.
// Concurrent callers which observed the same 401 therefore share one
// same-account refresh-token rotation.
func (s *savedLogin) RefreshAfterUnauthorized(ctx context.Context, rejected Credentials) (Credentials, error) {
	if s.provider == ProviderClaude {
		return s.claudeCredentials(ctx, rejected)
	}
	if rejected.AccessToken == "" || rejected.AccountID == "" {
		return Credentials{}, signInRequired(errors.New("rejected credentials are incomplete"))
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var result Credentials
	err := withAuthLock(s.home, func() error {
		auth, err := s.read()
		if err != nil {
			return err
		}
		if auth.Tokens.AccountID != rejected.AccountID {
			return signInRequired(errors.New("stored credentials belong to another account"))
		}
		if auth.Tokens.AccessToken != rejected.AccessToken {
			result = Credentials{AccessToken: auth.Tokens.AccessToken, AccountID: auth.Tokens.AccountID}
			return nil
		}
		if err := s.refresh(ctx, &auth); err != nil {
			return err
		}
		if auth.Tokens.AccountID != rejected.AccountID {
			return signInRequired(errors.New("refreshed credentials belong to another account"))
		}
		result = Credentials{AccessToken: auth.Tokens.AccessToken, AccountID: auth.Tokens.AccountID}
		return nil
	})
	return result, err
}

func (s *savedLogin) read() (authFile, error) {
	data, err := os.ReadFile(filepath.Join(s.home, "auth.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return authFile{}, signInRequired(err)
		}
		return authFile{}, temporary(fmt.Errorf("read credentials: %w", err))
	}
	var auth authFile
	if err := json.Unmarshal(data, &auth); err != nil {
		return authFile{}, signInRequired(err)
	}
	if auth.Tokens.AccessToken == "" || auth.Tokens.RefreshToken == "" || auth.Tokens.AccountID == "" {
		return authFile{}, signInRequired(errors.New("incomplete ChatGPT credentials"))
	}
	return auth, nil
}

func (s *savedLogin) refresh(ctx context.Context, auth *authFile) error {
	body, err := json.Marshal(map[string]string{"client_id": clientID, "grant_type": "refresh_token", "refresh_token": auth.Tokens.RefreshToken})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.token, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return temporary(fmt.Errorf("refresh credentials: %w", err))
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		failureBody, _ := io.ReadAll(io.LimitReader(resp.Body, maximumRefreshResponseBytes))
		var failure struct {
			Error string `json:"error"`
		}
		invalidGrant := resp.StatusCode == http.StatusBadRequest && json.Unmarshal(failureBody, &failure) == nil && failure.Error == "invalid_grant"
		if resp.StatusCode == http.StatusUnauthorized || invalidGrant {
			return signInRequired(fmt.Errorf("refresh credentials: status %d", resp.StatusCode))
		}
		return temporary(fmt.Errorf("refresh credentials: status %d", resp.StatusCode))
	}
	encoded, err := io.ReadAll(io.LimitReader(resp.Body, maximumRefreshResponseBytes+1))
	if err != nil {
		return temporary(fmt.Errorf("refresh credentials: %w", err))
	}
	if len(encoded) > maximumRefreshResponseBytes {
		return temporary(errors.New("refresh credentials: response is too large"))
	}
	var result struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(encoded, &result); err != nil {
		return temporary(fmt.Errorf("refresh credentials: %w", err))
	}
	if result.AccessToken == "" {
		return temporary(errors.New("refresh credentials: response has no access token"))
	}
	auth.Tokens.AccessToken = result.AccessToken
	if result.RefreshToken != "" {
		auth.Tokens.RefreshToken = result.RefreshToken
	}
	if result.IDToken != "" {
		if accountID := claim(result.IDToken, "chatgpt_account_id"); accountID != "" && accountID != auth.Tokens.AccountID {
			return signInRequired(errors.New("refreshed credentials belong to another account"))
		}
		auth.Tokens.IDToken = result.IDToken
	}
	auth.LastRefresh = time.Now().UTC()
	return writeAtomic(filepath.Join(s.home, "auth.json"), auth)
}

func expiresSoon(token string, margin time.Duration) bool {
	exp, ok := claimNumber(token, "exp")
	return !ok || time.Unix(exp, 0).Before(time.Now().Add(margin))
}

func claim(token, name string) string {
	payload, ok := jwtPayload(token)
	if !ok {
		return ""
	}
	value, _ := payload[name].(string)
	return value
}

func claimNumber(token, name string) (int64, bool) {
	payload, ok := jwtPayload(token)
	if !ok {
		return 0, false
	}
	value, ok := payload[name].(float64)
	return int64(value), ok
}

func jwtPayload(token string) (map[string]any, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, false
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, false
	}
	var payload map[string]any
	return payload, json.Unmarshal(data, &payload) == nil
}

func writeAtomic(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return writeAtomicData(path, data)
}

func writeAtomicData(path string, data []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".auth-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}
