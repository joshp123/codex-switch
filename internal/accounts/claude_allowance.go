package accounts

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

type claudeUsageWindow struct {
	Utilization *float64  `json:"utilization"`
	ResetsAt    time.Time `json:"resets_at"`
}

type claudeUsageResponse struct {
	FiveHour       *claudeUsageWindow `json:"five_hour"`
	SevenDay       *claudeUsageWindow `json:"seven_day"`
	SevenDayOpus   *claudeUsageWindow `json:"seven_day_opus"`
	SevenDaySonnet *claudeUsageWindow `json:"seven_day_sonnet"`
	SevenDayFable  *claudeUsageWindow `json:"seven_day_fable"`
	Limits         []struct {
		Kind     string    `json:"kind"`
		Percent  *float64  `json:"percent"`
		ResetsAt time.Time `json:"resets_at"`
		Scope    struct {
			Model struct {
				DisplayName string `json:"display_name"`
				ID          string `json:"id"`
			} `json:"model"`
		} `json:"scope"`
	} `json:"limits"`
}

func (usage claudeUsageResponse) sample(account PoolAccount, observedAt time.Time) (AllowanceSample, error) {
	sample := AllowanceSample{AccountID: account.AccountID, Plan: account.Plan, Allowed: true, ObservedAt: observedAt}
	for _, limit := range usage.Limits {
		duration := int64(7 * 86400)
		switch limit.Kind {
		case "session":
			duration = 5 * 3600
		case "weekly_all", "weekly_scoped":
		default:
			continue
		}
		if limit.Percent == nil || limit.ResetsAt.IsZero() {
			continue
		}
		window := ReportedWindow{UsedPercent: *limit.Percent, ResetsAt: limit.ResetsAt, DurationSeconds: duration, ObservedAt: observedAt}
		if limit.Kind == "weekly_scoped" {
			window.ModelID, window.ModelName = limit.Scope.Model.ID, limit.Scope.Model.DisplayName
			if window.ModelID == "" && window.ModelName == "" {
				return sample, fmt.Errorf("Claude model allowance has no model identity")
			}
		}
		if err := validateClaudeWindow(window); err != nil {
			return sample, err
		}
		sample.Windows = append(sample.Windows, window)
	}
	for _, old := range []struct {
		window   *claudeUsageWindow
		duration int64
		model    string
	}{
		{usage.FiveHour, 5 * 3600, ""}, {usage.SevenDay, 7 * 86400, ""},
		{usage.SevenDayOpus, 7 * 86400, "Opus"}, {usage.SevenDaySonnet, 7 * 86400, "Sonnet"}, {usage.SevenDayFable, 7 * 86400, "Fable"},
	} {
		if old.window == nil || old.window.Utilization == nil || old.window.ResetsAt.IsZero() {
			continue
		}
		window := ReportedWindow{UsedPercent: *old.window.Utilization, ResetsAt: old.window.ResetsAt, DurationSeconds: old.duration, ModelName: old.model, ObservedAt: observedAt}
		if err := validateClaudeWindow(window); err != nil {
			return sample, err
		}
		if !hasClaudeWindow(sample.Windows, window) {
			sample.Windows = append(sample.Windows, window)
		}
	}
	return sample, nil
}

func validateClaudeWindow(window ReportedWindow) error {
	if math.IsNaN(window.UsedPercent) || math.IsInf(window.UsedPercent, 0) || window.UsedPercent < 0 {
		return fmt.Errorf("Claude allowance has invalid utilization")
	}
	return nil
}

func ClaudeModelFamily(model string) string {
	model = strings.TrimSpace(strings.ToLower(model))
	model = strings.TrimPrefix(strings.TrimPrefix(model, "claude-"), "claude ")
	parts := strings.FieldsFunc(model, func(r rune) bool { return r == '-' || r == ' ' })
	if len(parts) == 0 {
		return ""
	}
	for _, part := range parts {
		switch part {
		case "opus", "sonnet", "haiku", "fable":
			return part
		}
	}
	return parts[0]
}

func claudeWindowMatchesModel(window ReportedWindow, model string) bool {
	if window.ModelID != "" {
		return canonicalClaudeModelID(window.ModelID) == canonicalClaudeModelID(model)
	}
	return window.ModelName == "" || ClaudeModelFamily(window.ModelName) == ClaudeModelFamily(model)
}

func canonicalClaudeModelID(model string) string {
	lastSeparator := strings.LastIndex(model, "-")
	if lastSeparator < 0 {
		return model
	}
	date := model[lastSeparator+1:]
	if len(date) == 8 {
		if _, err := strconv.Atoi(date); err == nil {
			return model[:lastSeparator]
		}
	}
	return model
}

func sameClaudeWindow(left, right ReportedWindow) bool {
	if left.DurationSeconds != right.DurationSeconds {
		return false
	}
	if left.ModelID != "" || right.ModelID != "" {
		return left.ModelID != "" && right.ModelID != "" && canonicalClaudeModelID(left.ModelID) == canonicalClaudeModelID(right.ModelID)
	}
	return ClaudeModelFamily(left.ModelName) == ClaudeModelFamily(right.ModelName)
}

func hasClaudeWindow(windows []ReportedWindow, desired ReportedWindow) bool {
	for _, window := range windows {
		if sameClaudeWindow(window, desired) {
			return true
		}
	}
	return false
}

func mergeClaudeAllowance(current, incoming AllowanceSample) AllowanceSample {
	current.Windows = append([]ReportedWindow(nil), current.Windows...)
	for _, incomingWindow := range incoming.Windows {
		found := false
		for index, window := range current.Windows {
			if !sameClaudeWindow(window, incomingWindow) {
				continue
			}
			found = true
			if !incomingWindow.ObservedAt.Before(window.ObservedAt) {
				current.Windows[index] = incomingWindow
			}
			break
		}
		if !found {
			current.Windows = append(current.Windows, incomingWindow)
		}
	}
	if incoming.ObservedAt.After(current.ObservedAt) {
		current.ObservedAt = incoming.ObservedAt
	}
	if current.AccountID == "" {
		current.AccountID = incoming.AccountID
	}
	if current.Plan == "" {
		current.Plan = incoming.Plan
	}
	return current
}

type ClaudeRestriction struct {
	ResetsAt   time.Time
	ObservedAt time.Time
	// Claim is Anthropic's representative claim on the 429, such as five_hour.
	Claim string `json:",omitempty"`
	// Throttle is a 429 that named no usage limit.
	Throttle bool `json:",omitempty"`
}

func claudeWindowRestrictionScope(window ReportedWindow) string {
	if window.ModelID != "" {
		return canonicalClaudeModelID(window.ModelID)
	}
	return ClaudeModelFamily(window.ModelName)
}

func mergeClaudeRestrictions(current, incoming map[string]ClaudeRestriction) map[string]ClaudeRestriction {
	merged := make(map[string]ClaudeRestriction, len(current)+len(incoming))
	for model, deadline := range current {
		merged[model] = deadline
	}
	for model, restriction := range incoming {
		if !restriction.ObservedAt.Before(merged[model].ObservedAt) {
			merged[model] = restriction
		}
	}
	return merged
}

func ClaudeUnavailableReason(fact AccountRoutingFact, model string, now time.Time) (string, time.Time) {
	var blockedUntil time.Time
	throttled := false
	for scope, restriction := range fact.ClaudeRestrictions {
		if (scope == "" || scope == ClaudeModelFamily(model) || scope == canonicalClaudeModelID(model)) && restriction.ResetsAt.After(blockedUntil) {
			blockedUntil, throttled = restriction.ResetsAt, restriction.Throttle
		}
	}
	for _, window := range fact.AllowanceSample.Windows {
		if claudeWindowMatchesModel(window, model) && (window.LimitReached || window.UsedPercent >= 100) && window.ResetsAt.After(blockedUntil) {
			blockedUntil, throttled = window.ResetsAt, false
		}
	}
	if blockedUntil.After(now) && throttled {
		return "Anthropic throttled it", blockedUntil
	}
	if blockedUntil.After(now) {
		return "Claude reports its usage limit reached", blockedUntil
	}
	return "", time.Time{}
}

// ClaudeAvailableAgain is when the account's reserve and Claude limits next
// allow model work, and Anthropic's name for the limit that ends then. found
// is false when no known reset frees the account.
func ClaudeAvailableAgain(fact AccountRoutingFact, model string, now time.Time) (availableAt time.Time, limitClaim string, found bool) {
	var resets []time.Time
	for _, window := range fact.AllowanceSample.Windows {
		resets = append(resets, window.ResetsAt)
	}
	for _, restriction := range fact.ClaudeRestrictions {
		resets = append(resets, restriction.ResetsAt)
	}
	slices.SortFunc(resets, time.Time.Compare)
	for _, reset := range resets {
		if !reset.After(now) {
			continue
		}
		if reason, _ := ClaudeUnavailableReason(fact, model, reset); reason == "" && ReserveUnavailableReason(fact, reset) == "" {
			return reset, claudeLimitClaimEndingAt(fact, model, reset), true
		}
	}
	return time.Time{}, "", false
}

// claudeLimitClaimEndingAt names a limit as Anthropic's representative claim,
// which Claude Code requires before it treats a 429 as a usage limit. It is ""
// for a throttle, which Claude Code retries.
func claudeLimitClaimEndingAt(fact AccountRoutingFact, model string, reset time.Time) string {
	claim := ""
	for _, window := range fact.AllowanceSample.Windows {
		if !window.ResetsAt.Equal(reset) || !claudeWindowMatchesModel(window, model) {
			continue
		}
		switch {
		case window.DurationSeconds != 7*86400:
			if claim == "" {
				claim = "five_hour"
			}
		case ClaudeModelFamily(window.ModelName) == "opus":
			return "seven_day_opus"
		case ClaudeModelFamily(window.ModelName) == "sonnet":
			return "seven_day_sonnet"
		default:
			claim = "seven_day"
		}
	}
	if claim != "" {
		return claim
	}
	for _, restriction := range fact.ClaudeRestrictions {
		if restriction.ResetsAt.Equal(reset) && restriction.Claim != "" {
			return restriction.Claim
		}
	}
	return ""
}

func ClaudeWeeklyAllowanceLeft(fact AccountRoutingFact, model string, now time.Time) (float64, bool) {
	free, known := 100.0, false
	for _, window := range fact.AllowanceSample.Windows {
		if window.DurationSeconds != 7*86400 || !window.ResetsAt.After(now) || !claudeWindowMatchesModel(window, model) {
			continue
		}
		free, known = math.Min(free, math.Max(0, 100-window.UsedPercent)), true
	}
	return free, known
}

func updateClaudeSharedState(fact *AccountRoutingFact, now time.Time) {
	if fact.AccountState == AccountSignInRequired || fact.AccountState == AccountCredentialsUnavailable {
		return
	}
	fact.AccountState = AccountReady
	if fact.AccountStateCause != nil && fact.AllowanceSample.ObservedAt.IsZero() {
		fact.AccountState = AccountUsageCheckFailed
	}
	reason, _ := ClaudeUnavailableReason(*fact, "", now)
	fact.AllowanceSample.Allowed, fact.AllowanceSample.LimitReached = reason == "", reason != ""
	if reason != "" {
		fact.AccountState = AccountLimited
	}
}
