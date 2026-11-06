package accounts

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const reserveFilename = "reserve-percent"

type AccountReserve struct {
	SoftPercent int
	HardPercent int
}

func (reserve AccountReserve) validate() error {
	if reserve.SoftPercent < 0 || reserve.SoftPercent > 100 || reserve.HardPercent < 0 || reserve.HardPercent > reserve.SoftPercent {
		return fmt.Errorf("reserves must be whole percentages from 0 to 100; hard must not exceed soft")
	}
	return nil
}

func readAccountReserve(accountDirectory string) (AccountReserve, error) {
	data, err := os.ReadFile(filepath.Join(accountDirectory, reserveFilename))
	if os.IsNotExist(err) {
		return AccountReserve{}, nil
	}
	if err != nil {
		return AccountReserve{}, err
	}
	fields := strings.Fields(string(data))
	// Existing saved reserves were hard cutoffs. Keep that cutoff on upgrade.
	if len(fields) == 1 {
		fields = append(fields, fields[0])
	}
	if len(fields) != 2 {
		return AccountReserve{}, fmt.Errorf("reserve file must contain soft and hard percentages")
	}
	soft, softErr := strconv.Atoi(fields[0])
	hard, hardErr := strconv.Atoi(fields[1])
	if softErr != nil || hardErr != nil {
		return AccountReserve{}, fmt.Errorf("reserves must be whole percentages from 0 to 100")
	}
	reserve := AccountReserve{SoftPercent: soft, HardPercent: hard}
	return reserve, reserve.validate()
}

// SetAccountReserve changes placement and cutover thresholds without a restart.
func SetAccountReserve(dataDirectory, target string, reserve AccountReserve) (PoolAccount, error) {
	if err := reserve.validate(); err != nil {
		return PoolAccount{}, err
	}
	var selected PoolAccount
	err := withPoolLock(dataDirectory, func() error {
		pool, err := ReadPool(dataDirectory)
		if err != nil {
			return err
		}
		for _, account := range pool {
			if string(account.Key) == target || account.DisplayName == target {
				selected = account
				data := []byte(fmt.Sprintf("%d %d\n", reserve.SoftPercent, reserve.HardPercent))
				return writeAtomicData(filepath.Join(dataDirectory, account.Folder, reserveFilename), data)
			}
		}
		return fmt.Errorf("unknown account %s", target)
	})
	return selected, err
}

// ReserveUnavailableReason applies to the whole account, including windows
// specific to another model. Expired windows no longer hold the account back.
func ReserveUnavailableReason(fact AccountRoutingFact, now time.Time) string {
	if reserveReached(fact, fact.Reserve.HardPercent, now) {
		return fmt.Sprintf("held back · %d%% reserve", fact.Reserve.HardPercent)
	}
	return ""
}

func SoftReserveReached(fact AccountRoutingFact, now time.Time) bool {
	return reserveReached(fact, fact.Reserve.SoftPercent, now)
}

func ReserveStatus(fact AccountRoutingFact, now time.Time) string {
	if reason := ReserveUnavailableReason(fact, now); reason != "" {
		return reason
	}
	if SoftReserveReached(fact, now) {
		return fmt.Sprintf("no new work · %d%% reserve", fact.Reserve.SoftPercent)
	}
	return ""
}

func reserveReached(fact AccountRoutingFact, percent int, now time.Time) bool {
	if percent == 0 {
		return false
	}
	for _, window := range fact.AllowanceSample.Windows {
		if window.ResetsAt.After(now) && window.UsedPercent >= float64(100-percent) {
			return true
		}
	}
	return false
}
