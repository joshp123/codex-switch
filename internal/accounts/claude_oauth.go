package accounts

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/joshp123/codex-switch/internal/store"
)

// SignInClaude obtains an independent rotating login; it never borrows another
// client's refresh token. With replacing set, it replaces that account's login.
func SignInClaude(dataDirectory string, replacing store.AccountKey) (PoolAccount, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	var randomBytes [32]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		return PoolAccount{}, err
	}
	verifier := base64.RawURLEncoding.EncodeToString(randomBytes[:])
	challengeBytes := sha256.Sum256([]byte(verifier))
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return PoolAccount{}, err
	}
	redirectURI := "http://localhost:" + fmt.Sprint(listener.Addr().(*net.TCPAddr).Port) + "/callback"
	codeReady := make(chan string, 1)
	mux := http.NewServeMux()
	mux.HandleFunc("/callback", func(response http.ResponseWriter, request *http.Request) {
		if request.URL.Query().Get("state") != verifier {
			http.Error(response, "Login state does not match.", http.StatusBadRequest)
			return
		}
		code := request.URL.Query().Get("code")
		if code == "" {
			http.Error(response, "Authorization code missing.", http.StatusBadRequest)
			return
		}
		select {
		case codeReady <- code:
		default:
		}
		fmt.Fprintln(response, "Claude account connected. You can close this tab.")
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go server.Serve(listener)
	defer server.Close()
	parameters := url.Values{"code": {"true"}, "client_id": {claudeOAuthClientID}, "response_type": {"code"}, "redirect_uri": {redirectURI},
		"scope": {claudeOAuthScopes}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challengeBytes[:])}, "code_challenge_method": {"S256"}, "state": {verifier}}
	authorizeURL := "https://claude.ai/oauth/authorize?" + parameters.Encode()
	fmt.Fprintf(os.Stderr, "Sign in to Claude:\n%s\n\nWaiting for the browser. You can also paste the final redirect URL or code#state here.\n", authorizeURL)
	if err := exec.Command("open", authorizeURL).Run(); err != nil {
		fmt.Fprintln(os.Stderr, "Open the login URL above in your browser.")
	}
	manual := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		if scanner.Scan() {
			manual <- strings.TrimSpace(scanner.Text())
		}
	}()
	var code string
	select {
	case <-ctx.Done():
		return PoolAccount{}, ctx.Err()
	case code = <-codeReady:
	case input := <-manual:
		state := ""
		if parsed, err := url.Parse(input); err == nil && parsed.Query().Get("code") != "" {
			code, state = parsed.Query().Get("code"), parsed.Query().Get("state")
		} else if strings.Contains(input, "#") {
			parts := strings.SplitN(input, "#", 2)
			code, state = parts[0], parts[1]
		} else {
			code = input
		}
		if state != "" && state != verifier {
			return PoolAccount{}, errors.New("Claude login state does not match")
		}
	}
	if code == "" {
		return PoolAccount{}, errors.New("Claude login returned no authorization code")
	}
	client := &http.Client{Timeout: 30 * time.Second}
	var token claudeTokenResponse
	if err := postClaudeOAuth(ctx, client, map[string]string{"grant_type": "authorization_code", "code": code, "state": verifier,
		"client_id": claudeOAuthClientID, "redirect_uri": redirectURI, "code_verifier": verifier}, &token); err != nil {
		return PoolAccount{}, err
	}
	if token.AccessToken == "" || token.RefreshToken == "" || token.ExpiresIn <= 0 {
		return PoolAccount{}, errors.New("Claude login returned incomplete tokens")
	}
	value := claudeAuthFile{Provider: ProviderClaude, AccessToken: token.AccessToken, RefreshToken: token.RefreshToken, ExpiresAt: time.Now().Add(time.Duration(token.ExpiresIn) * time.Second),
		AccountID: token.Account.UUID, OrganizationID: token.Organization.UUID, Email: token.Account.EmailAddress}
	if err := readClaudeProfile(ctx, client, &value); err != nil {
		return PoolAccount{}, err
	}
	if err := os.MkdirAll(dataDirectory, 0700); err != nil {
		return PoolAccount{}, err
	}
	staging, err := os.MkdirTemp(dataDirectory, ".login-*")
	if err != nil {
		return PoolAccount{}, err
	}
	if err := writeAtomic(filepath.Join(staging, "auth.json"), value); err != nil {
		_ = trash(staging)
		return PoolAccount{}, err
	}
	signedIn, err := ReadPoolAccount(staging, "staged account", 0)
	if err != nil {
		_ = trash(staging)
		return PoolAccount{}, err
	}
	var published PoolAccount
	err = withPoolLock(dataDirectory, func() error {
		var publishErr error
		published, publishErr = publishLogin(dataDirectory, staging, replacing, signedIn)
		return publishErr
	})
	if err != nil || replacing != "" {
		_ = trash(staging)
	}
	return published, err
}

func readClaudeProfile(ctx context.Context, client *http.Client, auth *claudeAuthFile) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.anthropic.com/api/oauth/profile", nil)
	if err != nil {
		return err
	}
	setClaudeOAuthHeaders(request, auth.AccessToken)
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Claude profile returned HTTP %d", response.StatusCode)
	}
	var profile struct {
		Account struct {
			UUID         string `json:"uuid"`
			Email        string `json:"email"`
			EmailAddress string `json:"email_address"`
		} `json:"account"`
		Organization struct {
			UUID string `json:"uuid"`
			Type string `json:"organization_type"`
			Tier string `json:"rate_limit_tier"`
		} `json:"organization"`
	}
	if err := json.NewDecoder(response.Body).Decode(&profile); err != nil {
		return err
	}
	if profile.Account.UUID != "" {
		auth.AccountID = profile.Account.UUID
	}
	if profile.Organization.UUID != "" {
		auth.OrganizationID = profile.Organization.UUID
	}
	if profile.Account.Email != "" {
		auth.Email = profile.Account.Email
	} else if profile.Account.EmailAddress != "" {
		auth.Email = profile.Account.EmailAddress
	}
	auth.Plan = profile.Organization.Type
	if strings.Contains(profile.Organization.Tier, "20x") {
		auth.Plan = "Max 20x"
	} else if strings.Contains(profile.Organization.Tier, "5x") {
		auth.Plan = "Max 5x"
	}
	if auth.AccountID == "" || auth.OrganizationID == "" || auth.Email == "" {
		return errors.New("Claude profile has incomplete account identity")
	}
	return nil
}
