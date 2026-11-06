package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	accountapi "github.com/joshp123/codex-switch/internal/accounts"
)

func writeAccountFixture(t *testing.T, home, userID, email, accessToken string) accountapi.PoolAccount {
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
	account, err := accountapi.ReadPoolAccount(home, filepath.Base(home), 0)
	if err != nil {
		t.Fatal(err)
	}
	return account
}

func accountAccessToken(t *testing.T, home string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	var value struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value.Tokens.AccessToken
}

func TestLoginRepairsTheNamedAccount(t *testing.T) {
	root := t.TempDir()
	writeAccountFixture(t, filepath.Join(root, "account-1"), "user-one", "one@example.com", "old-access")
	writeAccountFixture(t, filepath.Join(root, "account-2"), "user-two", "two@example.com", "other-access")
	fakeRoot := t.TempDir()
	staged := filepath.Join(fakeRoot, "staged")
	writeAccountFixture(t, staged, "user-one", "one@example.com", "new-access")
	fakeCodex := filepath.Join(fakeRoot, "codex")
	script := `#!/bin/sh
set -eu
test "$#" -eq 3
test "$1" = "login"
test "$2" = "-c"
test "$3" = 'cli_auth_credentials_store="file"'
cp "$CODEX_SWITCH_FAKE_AUTH" "$CODEX_HOME/auth.json"
`
	if err := os.WriteFile(fakeCodex, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_SWITCH_DATA_DIR", root)
	t.Setenv("CODEX_SWITCH_FAKE_CODEX", fakeCodex)
	t.Setenv("CODEX_SWITCH_FAKE_AUTH", filepath.Join(staged, "auth.json"))

	output, err := runLoginCLIProcess(t, "Codex 1")
	if err != nil {
		t.Fatalf("named login while runtime active: %v\n%s", err, output)
	}
	if got := accountAccessToken(t, filepath.Join(root, "account-1")); got != "new-access" {
		t.Fatalf("account-1 access token = %q", got)
	}
	if got := accountAccessToken(t, filepath.Join(root, "account-2")); got != "other-access" {
		t.Fatalf("account-2 access token = %q", got)
	}
}

func TestLoginCLIProcess(t *testing.T) {
	if os.Getenv("CODEX_SWITCH_LOGIN_CLI_PROCESS") != "1" {
		return
	}
	os.Args = []string{
		"codex-switch", "login", "--codex", os.Getenv("CODEX_SWITCH_FAKE_CODEX"),
	}
	if target := os.Getenv("CODEX_SWITCH_LOGIN_TARGET"); target != "" {
		os.Args = append(os.Args, target)
	}
	main()
}

func runLoginCLIProcess(t *testing.T, target string) ([]byte, error) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=^TestLoginCLIProcess$")
	command.Env = append(os.Environ(),
		"CODEX_SWITCH_LOGIN_CLI_PROCESS=1",
		"CODEX_SWITCH_LOGIN_TARGET="+target,
	)
	return command.CombinedOutput()
}
