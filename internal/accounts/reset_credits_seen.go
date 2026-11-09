package accounts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/joshp123/codex-switch/internal/store"
)

// OpenAI applies a spent credit at once, but its usage reading can still show
// the account at its limit for a while. A Mac that decided from that reading
// alone would find the pool still out, the spent credit gone, and spend the
// next account's credit for the same need. So each Mac remembers the credits
// it has seen available, and a credit that leaves its account before it
// expires, spent by this Mac, another Mac or the Codex app, counts as the pool
// refilling until the reading shows it.
//
// OpenAI's credit list gives no time a credit was spent and need not keep
// spent credits listed, so the time this Mac first sees one gone stands in
// for it.

// seenResetCredit is a credit listed as available at its account's last read.
type seenResetCredit struct {
	AccountKey store.AccountKey
	CreditID   string
	ExpiresAt  time.Time
}

// spentResetCredit is the latest credit this Mac saw leave its account before
// it expired. RefillsAt is when that account refilled by the reading that
// showed the credit gone: while a later reading gives the same time, the
// reading does not show the reset yet.
type spentResetCredit struct {
	AccountKey store.AccountKey
	CreditID   string
	SeenAt     time.Time
	RefillsAt  time.Time
}

type resetCreditsSeen struct {
	Available []seenResetCredit
	LastSpent spentResetCredit
}

// noteResetCredits records one account's credits as just read, with the
// allowance read beside them, and returns the latest credit seen spent. The
// proxy's account reads, each proxy generation and the reset command on this
// Mac share the record in the data directory.
func noteResetCredits(directory string, account PoolAccount, credits []ResetCredit, allowance AllowanceSample, now time.Time) (spentResetCredit, error) {
	var seen resetCreditsSeen
	err := withFileLock(filepath.Join(directory, "reset-credits-seen.lock"), func() error {
		path := filepath.Join(directory, "reset-credits-seen.json")
		if data, err := os.ReadFile(path); err == nil {
			if err := json.Unmarshal(data, &seen); err != nil {
				return err
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		var available []string
		for _, credit := range credits {
			if credit.usable(now) {
				available = append(available, credit.ID)
			}
		}
		var kept []seenResetCredit
		for _, credit := range seen.Available {
			switch {
			case credit.AccountKey != account.Key:
				kept = append(kept, credit)
			case !slices.Contains(available, credit.CreditID) && credit.ExpiresAt.After(now):
				refillsAt, _ := refillTime(allowance)
				seen.LastSpent = spentResetCredit{account.Key, credit.CreditID, now, refillsAt}
			}
		}
		for _, credit := range credits {
			if credit.usable(now) {
				kept = append(kept, seenResetCredit{account.Key, credit.ID, credit.ExpiresAt})
			}
		}
		seen.Available = kept
		data, err := json.Marshal(seen)
		if err != nil {
			return err
		}
		return writeAtomicData(path, data)
	})
	return seen.LastSpent, err
}
