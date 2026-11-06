package accounts

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/joshp123/codex-switch/internal/store"
)

func TestPublishLoginReplacesOnlyTheNamedMatchingAccount(t *testing.T) {
	root := t.TempDir()
	writeAccountFixture(t, filepath.Join(root, "account-1"), "user-one", "one@example.com", "old-access")
	writeAccountFixture(t, filepath.Join(root, "account-2"), "user-two", "two@example.com", "other-access")
	staging := filepath.Join(root, ".login-staging")
	added := writeAccountFixture(t, staging, "user-one", "one@example.com", "new-access")

	published, err := publishLogin(root, staging, "chatgpt:one@example.com", added)
	if err != nil {
		t.Fatal(err)
	}
	if published.Folder != "account-1" || published.Index != 1 {
		t.Fatalf("published = %#v", published)
	}
	if got := accountAccessToken(t, filepath.Join(root, "account-1")); got != "new-access" {
		t.Fatalf("active access token = %q", got)
	}
	if got := accountAccessToken(t, staging); got != "new-access" {
		t.Fatalf("staged access token = %q", got)
	}
	if got := accountAccessToken(t, filepath.Join(root, "account-2")); got != "other-access" {
		t.Fatalf("unrelated access token = %q", got)
	}
}

func TestPublishLoginRejectsInvalidNamedTargetsWithoutChangingCredentials(t *testing.T) {
	for _, test := range []struct {
		name        string
		target      string
		stagedUser  string
		stagedEmail string
		wantError   string
	}{
		{
			name: "wrong identity", target: "chatgpt:one@example.com",
			stagedUser: "user-two", stagedEmail: "two@example.com",
			wantError: "different ChatGPT account",
		},
		{
			name: "unknown account", target: "chatgpt:two@example.com",
			stagedUser: "user-one", stagedEmail: "one@example.com",
			wantError: "unknown account chatgpt:two@example.com",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			writeAccountFixture(t, filepath.Join(root, "account-1"), "user-one", "one@example.com", "old-access")
			staging := filepath.Join(root, ".login-staging")
			added := writeAccountFixture(t, staging, test.stagedUser, test.stagedEmail, "new-access")

			if _, err := publishLogin(root, staging, store.AccountKey(test.target), added); err == nil || !strings.Contains(err.Error(), test.wantError) {
				t.Fatalf("error = %v", err)
			}
			if got := accountAccessToken(t, filepath.Join(root, "account-1")); got != "old-access" {
				t.Fatalf("active access token = %q", got)
			}
			if got := accountAccessToken(t, staging); got != "new-access" {
				t.Fatalf("staged access token = %q", got)
			}
		})
	}
}

func TestPublishLoginAddsSixthAndTwentiethAccounts(t *testing.T) {
	for _, count := range []int{6, 20} {
		t.Run(strconv.Itoa(count), func(t *testing.T) {
			root := t.TempDir()
			for index := 1; index < count; index++ {
				writeAccountFixture(t, filepath.Join(root, fmt.Sprintf("account-%d", index)), fmt.Sprintf("user-%d", index), fmt.Sprintf("user-%d@example.com", index), "old-access")
			}
			staging := filepath.Join(root, ".login-staging")
			added := writeAccountFixture(t, staging, fmt.Sprintf("user-%d", count), fmt.Sprintf("user-%d@example.com", count), "new-access")

			published, err := publishLogin(root, staging, "", added)
			if err != nil {
				t.Fatal(err)
			}
			wantName := fmt.Sprintf("account-%d", count)
			if published.Folder != wantName || published.Index != count || published.Key != store.AccountKey(fmt.Sprintf("chatgpt:user-%d@example.com", count)) {
				t.Fatalf("published = %#v", published)
			}
			if got := accountAccessToken(t, filepath.Join(root, wantName)); got != "new-access" {
				t.Fatalf("published access token = %q", got)
			}
			if _, err := os.Stat(staging); !os.IsNotExist(err) {
				t.Fatalf("staging directory remains: %v", err)
			}
		})
	}
}

// The pool knows an account by its provider and email, so a second login that
// resolves to an account already in the pool is refused.
func TestPublishLoginRefusesAnAccountAlreadyInThePool(t *testing.T) {
	root := t.TempDir()
	writeAccountFixture(t, filepath.Join(root, "account-1"), "user-one", "one@example.com", "old-access")
	staging := filepath.Join(root, ".login-staging")
	added := writeAccountFixture(t, staging, "user-other", "One@Example.com", "new-access")

	_, err := publishLogin(root, staging, "", added)
	if err == nil || !strings.Contains(err.Error(), "already signed in as Codex 1") {
		t.Fatalf("error = %v", err)
	}
	if entries, _ := filepath.Glob(filepath.Join(root, "account-*")); len(entries) != 1 {
		t.Fatalf("pool folders = %v", entries)
	}
}

func TestAtomicAuthReplacementFailureLeavesTheActiveCredentialReadable(t *testing.T) {
	root := t.TempDir()
	active := filepath.Join(root, "account-1")
	staging := filepath.Join(root, ".login-staging")
	writeAccountFixture(t, active, "user-one", "one@example.com", "old-access")
	writeAccountFixture(t, staging, "user-one", "one@example.com", "new-access")

	errInjected := errors.New("injected rename failure")
	err := replaceAuthFileWithRename(active, filepath.Join(staging, "auth.json"), func(_, _ string) error {
		return errInjected
	})
	if !errors.Is(err, errInjected) {
		t.Fatalf("error = %v", err)
	}
	if got := accountAccessToken(t, active); got != "old-access" {
		t.Fatalf("active access token = %q", got)
	}
	if got := accountAccessToken(t, staging); got != "new-access" {
		t.Fatalf("staged access token = %q", got)
	}
	entries, err := os.ReadDir(active)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "auth.json" {
		t.Fatalf("active directory = %#v", entries)
	}
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
