package accounts

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/joshp123/codex-switch/internal/store"
)

type Provider string

const (
	ProviderCodex   Provider = ""
	ProviderChatGPT Provider = ProviderCodex
	ProviderClaude  Provider = "claude"
)

// PoolAccount is one signed-in subscription account. Pool accounts live in
// account-N folders in the data directory, each holding an auth.json.
type PoolAccount struct {
	Provider       Provider
	OrganizationID string
	Plan           string
	// Key identifies the account on every Mac; see AccountKeyFor.
	Key store.AccountKey
	// Folder is where its login is saved, such as account-3, and Index its
	// number. Folders are numbered in sign-in order on each Mac.
	Folder         string
	Index          int
	DisplayName    string
	ProviderNumber int
	Reserve        AccountReserve
	// ID identifies the subscription user within its provider.
	ID string
	// AccountID is the immutable subscription identity. For ChatGPT it is sent
	// as ChatGPT-Account-ID; for Claude it joins the account and organization IDs.
	AccountID string
	Email     string
}

// ReadPool returns every signed-in account in dataDirectory, ordered by
// folder number. A missing data directory is an empty pool.
func ReadPool(dataDirectory string) ([]PoolAccount, error) {
	entries, err := os.ReadDir(dataDirectory)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var result []PoolAccount
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "account-") {
			continue
		}
		index, err := strconv.Atoi(strings.TrimPrefix(entry.Name(), "account-"))
		if err != nil || index < 1 {
			continue
		}
		account, err := readPoolAccountFiles(filepath.Join(dataDirectory, entry.Name()), entry.Name(), index)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result = append(result, account)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Index < result[j].Index })
	folders := make(map[store.AccountKey]string, len(result))
	for index, account := range result {
		if folder, duplicate := folders[account.Key]; duplicate {
			return nil, fmt.Errorf("%s and %s in %s are both signed in to %s; move one of them to the Trash", folder, account.Folder, dataDirectory, account.Key)
		}
		folders[account.Key] = account.Folder
		result[index].DisplayName, result[index].ProviderNumber = AccountDisplayName(result, account.Key)
	}
	return result, nil
}

// AccountKeyFor names a subscription account by its provider and email, such
// as "chatgpt:you@example.com". One email can hold both a ChatGPT and a Claude
// subscription, so the provider is part of the key.
func AccountKeyFor(provider Provider, email string) store.AccountKey {
	name := "chatgpt"
	if provider == ProviderClaude {
		name = "claude"
	}
	return store.AccountKey(name + ":" + strings.ToLower(email))
}

// poolAccountFiles keeps each account as last parsed, with the identity of
// the files it came from. Routing reads the pool on every request; a file is
// opened again only after it changes.
var poolAccountFiles = struct {
	sync.Mutex
	byDirectory map[string]parsedPoolAccount
}{byDirectory: map[string]parsedPoolAccount{}}

type parsedPoolAccount struct {
	files   string
	account PoolAccount
}

// readPoolAccountFiles reads an account's identity and reserve, from memory
// while its auth.json and reserve-percent are unchanged.
func readPoolAccountFiles(accountDirectory, folder string, index int) (PoolAccount, error) {
	authentication, err := os.Stat(filepath.Join(accountDirectory, "auth.json"))
	if err != nil {
		return PoolAccount{}, err
	}
	files := fileIdentity(authentication)
	if reserve, err := os.Stat(filepath.Join(accountDirectory, reserveFilename)); err == nil {
		files += " " + fileIdentity(reserve)
	} else if !os.IsNotExist(err) {
		return PoolAccount{}, fmt.Errorf("read %s reserve: %w", folder, err)
	}
	poolAccountFiles.Lock()
	parsed, known := poolAccountFiles.byDirectory[accountDirectory]
	poolAccountFiles.Unlock()
	if known && parsed.files == files && parsed.account.Folder == folder && parsed.account.Index == index {
		return parsed.account, nil
	}
	account, err := ReadPoolAccount(accountDirectory, folder, index)
	if err != nil {
		return PoolAccount{}, err
	}
	if account.Reserve, err = readAccountReserve(accountDirectory); err != nil {
		return PoolAccount{}, fmt.Errorf("read %s reserve: %w", folder, err)
	}
	poolAccountFiles.Lock()
	poolAccountFiles.byDirectory[accountDirectory] = parsedPoolAccount{files: files, account: account}
	poolAccountFiles.Unlock()
	return account, nil
}

func fileIdentity(info os.FileInfo) string {
	identity := fmt.Sprintf("%d %d", info.Size(), info.ModTime().UnixNano())
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		identity += fmt.Sprintf(" %d:%d", stat.Dev, stat.Ino)
	}
	return identity
}

// ReadPoolAccount reads the provider identity from the auth.json in
// accountDirectory.
func ReadPoolAccount(accountDirectory, folder string, index int) (PoolAccount, error) {
	data, err := os.ReadFile(filepath.Join(accountDirectory, "auth.json"))
	if err != nil {
		return PoolAccount{}, err
	}
	var value struct {
		Provider Provider `json:"provider"`
		Tokens   struct {
			AccountID string `json:"account_id"`
			IDToken   string `json:"id_token"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return PoolAccount{}, err
	}
	if value.Provider == ProviderClaude {
		return readClaudePoolAccount(data, folder, index)
	}
	if value.Tokens.AccountID == "" {
		return PoolAccount{}, fmt.Errorf("%s has no ChatGPT account ID", folder)
	}
	email, userID, err := decodeIDTokenIdentity(value.Tokens.IDToken)
	if err != nil {
		return PoolAccount{}, fmt.Errorf("read %s identity: %w", folder, err)
	}
	identity := userID
	if identity == "" {
		identity = value.Tokens.AccountID + "|" + email
	}
	return PoolAccount{
		Key: AccountKeyFor(ProviderChatGPT, email), Folder: folder, Index: index, ID: identity,
		AccountID: value.Tokens.AccountID, Email: email,
	}, nil
}

func decodeIDTokenIdentity(token string) (string, string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", "", errors.New("ID token is not a JWT")
	}
	data, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", "", fmt.Errorf("decode ID token claims: %w", err)
	}
	var claims struct {
		Email string `json:"email"`
		Auth  struct {
			ChatGPTUserID string `json:"chatgpt_user_id"`
			UserID        string `json:"user_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(data, &claims); err != nil {
		return "", "", fmt.Errorf("decode ID token identity: %w", err)
	}
	if claims.Email == "" {
		return "", "", errors.New("ID token has no email")
	}
	userID := claims.Auth.ChatGPTUserID
	if userID == "" {
		userID = claims.Auth.UserID
	}
	return claims.Email, userID, nil
}

// AccountDisplayName derives a name and provider-local number from pool order.
// A key outside the current pool belongs to a removed account.
func AccountDisplayName(pool []PoolAccount, key store.AccountKey) (string, int) {
	numbers := map[Provider]int{}
	for _, account := range pool {
		numbers[account.Provider]++
		if account.Key == key {
			provider := "Codex"
			if account.Provider == ProviderClaude {
				provider = "Claude"
			}
			number := numbers[account.Provider]
			return fmt.Sprintf("%s %d", provider, number), number
		}
	}
	return "removed account", 0
}
