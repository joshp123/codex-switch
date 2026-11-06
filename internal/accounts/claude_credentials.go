package accounts

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	claudeOAuthClientID = "9d1c250a-e61b-44d9-88ed-5944d1962f5e"
	claudeOAuthTokenURL = "https://platform.claude.com/v1/oauth/token"
	claudeOAuthScopes   = "user:profile user:inference user:sessions:claude_code user:mcp_servers user:file_upload"
)

type claudeAuthFile struct {
	Provider       Provider  `json:"provider"`
	AccessToken    string    `json:"access_token"`
	RefreshToken   string    `json:"refresh_token"`
	ExpiresAt      time.Time `json:"expires_at"`
	AccountID      string    `json:"account_id"`
	OrganizationID string    `json:"organization_id"`
	Email          string    `json:"email"`
	Plan           string    `json:"plan"`
}

func (auth claudeAuthFile) credentials() Credentials {
	return Credentials{AccessToken: auth.AccessToken, AccountID: auth.AccountID + "|" + auth.OrganizationID}
}

// providerOfAccountID tells a Claude account ID, which joins the account and
// organization IDs, from a ChatGPT one.
func providerOfAccountID(accountID string) Provider {
	if strings.Contains(accountID, "|") {
		return ProviderClaude
	}
	return ProviderChatGPT
}

func readClaudePoolAccount(data []byte, folder string, index int) (PoolAccount, error) {
	var auth claudeAuthFile
	if err := json.Unmarshal(data, &auth); err != nil {
		return PoolAccount{}, err
	}
	if auth.AccountID == "" || auth.OrganizationID == "" || auth.Email == "" {
		return PoolAccount{}, fmt.Errorf("%s has incomplete Claude identity", folder)
	}
	identity := auth.credentials().AccountID
	return PoolAccount{Key: AccountKeyFor(ProviderClaude, auth.Email), Folder: folder, Index: index, Provider: ProviderClaude, ID: identity,
		AccountID: identity, OrganizationID: auth.OrganizationID, Email: auth.Email, Plan: auth.Plan}, nil
}

func (s *savedLogin) readClaudeAuth() (claudeAuthFile, error) {
	data, err := os.ReadFile(filepath.Join(s.home, "auth.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return claudeAuthFile{}, signInRequired(err)
		}
		return claudeAuthFile{}, temporary(err)
	}
	var auth claudeAuthFile
	if err := json.Unmarshal(data, &auth); err != nil {
		return auth, signInRequired(err)
	}
	if auth.AccessToken == "" || auth.RefreshToken == "" || auth.AccountID == "" || auth.OrganizationID == "" || auth.ExpiresAt.IsZero() {
		return auth, signInRequired(errors.New("incomplete Claude credentials"))
	}
	return auth, nil
}

func (s *savedLogin) currentClaudeCredentials() (Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	auth, err := s.readClaudeAuth()
	if err != nil {
		return Credentials{}, err
	}
	if !auth.ExpiresAt.After(time.Now().Add(time.Minute)) {
		return Credentials{}, signInRequired(errors.New("stored Claude token expired"))
	}
	return auth.credentials(), nil
}

func (s *savedLogin) claudeCredentials(ctx context.Context, rejected Credentials) (Credentials, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result Credentials
	err := withAuthLock(s.home, func() error {
		auth, err := s.readClaudeAuth()
		if err != nil {
			return err
		}
		if rejected.AccountID != "" && rejected.AccountID != auth.credentials().AccountID {
			return signInRequired(errors.New("stored Claude login belongs to another account"))
		}
		forceRefresh := rejected.AccessToken != "" && rejected.AccessToken == auth.AccessToken
		if forceRefresh || !auth.ExpiresAt.After(time.Now().Add(time.Minute)) {
			var refreshed claudeTokenResponse
			if err := postClaudeOAuth(ctx, s.client, map[string]string{
				"grant_type": "refresh_token", "client_id": claudeOAuthClientID,
				"refresh_token": auth.RefreshToken, "scope": claudeOAuthScopes,
			}, &refreshed); err != nil {
				return err
			}
			if refreshed.AccessToken == "" || refreshed.RefreshToken == "" || refreshed.ExpiresIn <= 0 {
				return temporary(errors.New("Claude token refresh returned incomplete credentials"))
			}
			if refreshed.Account.UUID != "" && refreshed.Account.UUID != auth.AccountID ||
				refreshed.Organization.UUID != "" && refreshed.Organization.UUID != auth.OrganizationID {
				return signInRequired(errors.New("refreshed Claude login belongs to another account"))
			}
			auth.AccessToken, auth.RefreshToken = refreshed.AccessToken, refreshed.RefreshToken
			auth.ExpiresAt = time.Now().Add(time.Duration(refreshed.ExpiresIn) * time.Second)
			if err := writeAtomic(filepath.Join(s.home, "auth.json"), auth); err != nil {
				return temporary(err)
			}
		}
		result = auth.credentials()
		return nil
	})
	return result, err
}

type claudeTokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	Account      struct {
		UUID         string `json:"uuid"`
		EmailAddress string `json:"email_address"`
	} `json:"account"`
	Organization struct {
		UUID string `json:"uuid"`
	} `json:"organization"`
}

func postClaudeOAuth(ctx context.Context, client *http.Client, body any, result any) error {
	encoded, err := json.Marshal(body)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, claudeOAuthTokenURL, bytes.NewReader(encoded))
	if err != nil {
		return err
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return temporary(fmt.Errorf("Claude token request: %w", err))
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		io.Copy(io.Discard, io.LimitReader(response.Body, maximumRefreshResponseBytes))
		err := fmt.Errorf("Claude token request returned HTTP %d", response.StatusCode)
		if response.StatusCode == 400 || response.StatusCode == 401 || response.StatusCode == 403 {
			return signInRequired(err)
		}
		return temporary(err)
	}
	return json.NewDecoder(io.LimitReader(response.Body, maximumRefreshResponseBytes)).Decode(result)
}
