package accounts

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/joshp123/codex-switch/internal/store"
)

// SignIn runs Codex's browser login into a staging directory, then adds the
// signed-in account to the pool. With replacing set, it replaces that
// account's login instead, and only if the browser signed in to the same user.
func SignIn(dataDirectory, codexExecutable string, replacing store.AccountKey) (PoolAccount, error) {
	if err := os.MkdirAll(dataDirectory, 0o700); err != nil {
		return PoolAccount{}, err
	}
	staging, err := os.MkdirTemp(dataDirectory, ".login-*")
	if err != nil {
		return PoolAccount{}, err
	}
	login := exec.Command(codexExecutable, "login", "-c", `cli_auth_credentials_store="file"`)
	login.Env = append(os.Environ(), "CODEX_HOME="+staging)
	login.Stdin, login.Stdout, login.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := login.Run(); err != nil {
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
	if err != nil {
		_ = trash(staging)
		return PoolAccount{}, err
	}
	if replacing != "" {
		if err := trash(staging); err != nil {
			log.Printf("warning: temporary login files remain at %s: %v", staging, err)
		}
	}
	return published, nil
}

func publishLogin(dataDirectory, staging string, replacing store.AccountKey, signedIn PoolAccount) (PoolAccount, error) {
	existing, err := ReadPool(dataDirectory)
	if err != nil {
		return PoolAccount{}, err
	}
	if replacing != "" {
		var selected *PoolAccount
		for index := range existing {
			if existing[index].Key == replacing {
				selected = &existing[index]
				break
			}
		}
		if selected == nil {
			return PoolAccount{}, fmt.Errorf("unknown account %s", replacing)
		}
		if signedIn.Provider != selected.Provider || signedIn.ID != selected.ID {
			providerName := "ChatGPT"
			if signedIn.Provider == ProviderClaude {
				providerName = "Claude"
			}
			return PoolAccount{}, fmt.Errorf("the browser signed in to a different %s account; %s was not changed", providerName, selected.DisplayName)
		}
		active := filepath.Join(dataDirectory, selected.Folder)
		if err := withAuthLock(active, func() error {
			return replaceAuthFile(active, filepath.Join(staging, "auth.json"))
		}); err != nil {
			return PoolAccount{}, err
		}
		signedIn.Folder = selected.Folder
		signedIn.Index = selected.Index
		return signedIn, nil
	}

	for _, account := range existing {
		if account.Key == signedIn.Key || account.Provider == signedIn.Provider && account.ID == signedIn.ID {
			return PoolAccount{}, fmt.Errorf("%s is already signed in as %s; to sign it in again, run codex-switch login \"%s\"", signedIn.Email, account.DisplayName, account.DisplayName)
		}
	}
	database, err := store.OpenStoreDatabase(filepath.Join(dataDirectory, "codex-switch.db"))
	if err != nil {
		return PoolAccount{}, err
	}
	defer database.CloseStoreDatabase()
	for _, account := range existing {
		if err := rememberPoolAccount(database, account); err != nil {
			return PoolAccount{}, err
		}
	}
	index, err := database.NextAccountNumber()
	if err != nil {
		return PoolAccount{}, err
	}
	signedIn.Folder = fmt.Sprintf("account-%d", index)
	if err := rememberPoolAccount(database, signedIn); err != nil {
		return PoolAccount{}, err
	}
	if err := os.Rename(staging, filepath.Join(dataDirectory, signedIn.Folder)); err != nil {
		return PoolAccount{}, err
	}
	signedIn.Index = index
	return signedIn, nil
}

func replaceAuthFile(accountDirectory, stagedPath string) error {
	if err := replaceAuthFileWithRename(accountDirectory, stagedPath, os.Rename); err != nil {
		return err
	}
	directory, err := os.Open(accountDirectory)
	if err != nil {
		log.Printf("warning: credentials were updated but the account directory could not be synced: %v", err)
		return nil
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		log.Printf("warning: credentials were updated but the account directory could not be synced: %v", err)
	}
	return nil
}

func replaceAuthFileWithRename(accountDirectory, stagedPath string, rename func(string, string) error) error {
	data, err := os.ReadFile(stagedPath)
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(accountDirectory, ".auth-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return rename(temporaryPath, filepath.Join(accountDirectory, "auth.json"))
}

// RemoveAccount takes the account named by its display name or key out of the
// pool, forgets its task pins and moves its saved login to the Trash. It
// returns the removed account and how many task pins it had.
func RemoveAccount(dataDirectory, target string) (PoolAccount, int64, error) {
	var selected *PoolAccount
	var removed string
	var removedPins int64
	err := withPoolLock(dataDirectory, func() error {
		pool, err := ReadPool(dataDirectory)
		if err != nil {
			return err
		}
		for index := range pool {
			if string(pool[index].Key) == target || pool[index].DisplayName == target {
				selected = &pool[index]
				break
			}
		}
		if selected == nil {
			return fmt.Errorf("unknown account %s", target)
		}
		removed = filepath.Join(dataDirectory, fmt.Sprintf(".remove-%s-%d", selected.Folder, time.Now().Unix()))
		active := filepath.Join(dataDirectory, selected.Folder)
		if err := withAuthLock(active, func() error {
			return os.Rename(active, removed)
		}); err != nil {
			return err
		}
		database, err := store.OpenStoreDatabase(filepath.Join(dataDirectory, "codex-switch.db"))
		if err != nil {
			return errors.Join(err, restoreAccountAfterFailedRemoval(removed, active))
		}
		for _, account := range pool {
			if err := rememberPoolAccount(database, account); err != nil {
				database.CloseStoreDatabase()
				return errors.Join(err, restoreAccountAfterFailedRemoval(removed, active))
			}
		}
		removedPins, err = database.RemoveAccountRoutingState(selected.Key)
		closeErr := database.CloseStoreDatabase()
		if err != nil || closeErr != nil {
			return errors.Join(err, closeErr, restoreAccountAfterFailedRemoval(removed, active))
		}
		return nil
	})
	if err != nil {
		return PoolAccount{}, 0, err
	}
	if err := trash(removed); err != nil {
		return PoolAccount{}, 0, err
	}
	return *selected, removedPins, nil
}

func rememberPoolAccount(database *store.StoreDatabase, account PoolAccount) error {
	return database.RememberAccountIdentity(store.AccountIdentity{
		AccountFolder: account.Folder, AccountKey: account.Key, ProviderAccountID: account.AccountID, Email: account.Email,
	})
}

// RecordAccountKeys records which folder holds which account, gives accounts
// recorded by earlier builds their key, and fills the key into records that
// earlier builds wrote with only a folder name. It returns how many records it
// filled. The proxy runs it whenever it reads the pool: an earlier build may
// write records while an update switches proxies, or after a rollback.
func RecordAccountKeys(dataDirectory string, database *store.StoreDatabase) (int64, error) {
	pool, err := ReadPool(dataDirectory)
	if err != nil {
		return 0, err
	}
	return recordAccountKeys(pool, database)
}

func recordAccountKeys(pool []PoolAccount, database *store.StoreDatabase) (int64, error) {
	identities, err := database.ReadAccountIdentities()
	if err != nil {
		return 0, err
	}
	recorded := make(map[string]store.AccountIdentity, len(identities))
	for _, identity := range identities {
		recorded[identity.AccountFolder] = identity
	}
	for _, account := range pool {
		identity, known := recorded[account.Folder]
		if known && identity.AccountKey == account.Key && identity.Email == account.Email {
			continue
		}
		if err := rememberPoolAccount(database, account); err != nil {
			return 0, err
		}
		delete(recorded, account.Folder)
	}
	for _, identity := range recorded {
		if identity.AccountKey != "" {
			continue
		}
		identity.AccountKey = AccountKeyFor(providerOfAccountID(identity.ProviderAccountID), identity.Email)
		if err := database.RememberAccountIdentity(identity); err != nil {
			return 0, err
		}
	}
	return database.FillAccountKeys()
}

func restoreAccountAfterFailedRemoval(removedPath, activePath string) error {
	if err := os.Rename(removedPath, activePath); err != nil {
		return fmt.Errorf("restore account after failed removal: %w", err)
	}
	return nil
}

// trash moves saved credentials to the macOS Trash rather than deleting them.
func trash(path string) error {
	return exec.Command("trash", path).Run()
}
