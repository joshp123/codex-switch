package accounts

import "time"

type AccountState string

const (
	AccountReady                  AccountState = "ready"
	AccountUsageCheckPending      AccountState = "usage-check-pending"
	AccountSignInRequired         AccountState = "sign-in-required"
	AccountCredentialsUnavailable AccountState = "credentials-unavailable"
	AccountLimited                AccountState = "limited"
	AccountUsageCheckFailed       AccountState = "usage-check-failed"
)

func (s AccountState) Eligible() bool {
	return s == AccountReady || s == AccountUsageCheckPending || s == AccountUsageCheckFailed
}

type ReportedWindow struct {
	ObservedAt      time.Time
	LimitReached    bool
	ModelID         string
	ModelName       string
	UsedPercent     float64
	DurationSeconds int64
	ResetsAt        time.Time
}

type AllowanceSample struct {
	AccountID             string
	Plan                  string
	Allowed               bool
	LimitReached          bool
	ObservedAt            time.Time
	AvailableResetCredits *uint32
	// CreditBalance is the account's usage credit balance; nil when OpenAI
	// reports none.
	CreditBalance *float64
	Windows       []ReportedWindow
}
