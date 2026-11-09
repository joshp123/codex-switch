package accounts

import (
	"cmp"
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

// claudeResetGrantStatus is Anthropic's list of an account's stored resets.
// Anthropic lists them only to a Claude Code it accepts; to any other client
// it answers that the account is not eligible, with the reason.
type claudeResetGrantStatus struct {
	Eligible         *bool              `json:"eligible"`
	IneligibleReason string             `json:"ineligible_reason"`
	Grants           []claudeResetGrant `json:"grants"`
}

type claudeResetGrant struct {
	ResetsLeft *uint32    `json:"resets_left"`
	EndsAt     *time.Time `json:"ends_at"`
	// Paused is a grant Anthropic has suspended: it cannot be spent.
	Paused bool `json:"paused"`
	// Type is session, weekly or full.
	Type string `json:"type"`
	// Clears names the limits a reset refills, as Anthropic names them on a
	// 429: five_hour, seven_day, seven_day_overage_included (Fable's).
	Clears []string `json:"clears"`
	// GivesBackWeekly marks a Fable reset: it also returns what Fable used
	// to the weekly allowance.
	GivesBackWeekly bool `json:"gives_back_weekly"`
}

// kind names what a reset from the grant gives back, by the rule Claude Code
// applies to the same fields.
func (grant claudeResetGrant) kind() ResetKind {
	switch {
	case grant.GivesBackWeekly:
		return ResetFable
	case grant.Type == "session":
		return ResetFiveHour
	case grant.Type == "weekly":
		return ResetWeekly
	case grant.Type == "full":
		return ResetFull
	}
	fiveHour := slices.Contains(grant.Clears, "five_hour")
	other := slices.ContainsFunc(grant.Clears, func(limit string) bool { return limit != "five_hour" })
	switch {
	case fiveHour && !other:
		return ResetFiveHour
	case other && !fiveHour:
		return ResetWeekly
	}
	return ResetFull
}

type claudeUsageResponse struct {
	ResetGrants    *claudeResetGrantStatus `json:"cedar_ember"`
	FiveHour       *claudeUsageWindow      `json:"five_hour"`
	SevenDay       *claudeUsageWindow      `json:"seven_day"`
	SevenDayOpus   *claudeUsageWindow      `json:"seven_day_opus"`
	SevenDaySonnet *claudeUsageWindow      `json:"seven_day_sonnet"`
	SevenDayFable  *claudeUsageWindow      `json:"seven_day_fable"`
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
	if usage.ResetGrants != nil && usage.ResetGrants.Eligible != nil {
		var count uint32
		var held []StoredReset
		for _, grant := range usage.ResetGrants.Grants {
			if grant.Paused || (grant.EndsAt != nil && !grant.EndsAt.After(observedAt)) {
				continue
			}
			if grant.ResetsLeft == nil {
				return sample, fmt.Errorf("Claude reset grant has no remaining count")
			}
			reset := StoredReset{Kind: grant.kind(), Count: *grant.ResetsLeft}
			if grant.EndsAt != nil {
				reset.ExpiresAt = *grant.EndsAt
			}
			held = append(held, reset)
			count += *grant.ResetsLeft
		}
		sample.AvailableResetCredits, sample.StoredResets, sample.StoredResetsReadAt = &count, groupStoredResets(held), observedAt
		if !*usage.ResetGrants.Eligible {
			sample.NoResetsReason = cmp.Or(usage.ResetGrants.IneligibleReason, "no reason given")
		}
	}
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
	// Only a usage read that Anthropic answered with its list replaces the
	// stored resets. Response headers carry none, and neither does a reading
	// saved by a proxy built before the list, which may run beside this one
	// during an update.
	if incoming.StoredResetsReadAt.After(current.StoredResetsReadAt) {
		current.AvailableResetCredits, current.StoredResets = incoming.AvailableResetCredits, incoming.StoredResets
		current.NoResetsReason, current.StoredResetsReadAt = incoming.NoResetsReason, incoming.StoredResetsReadAt
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

// claudeThrottleHold is how long a throttle without a Retry-After keeps an
// account from the model.
const claudeThrottleHold = time.Minute

// endsAt is when the restriction stops blocking. One saved before 1 November
// 2026 names neither a limit nor a throttle: it may come from a throttle read
// as a usage limit, so it is held as a throttle.
func (restriction ClaudeRestriction) endsAt() time.Time {
	if restriction.Claim == "" && !restriction.Throttle && restriction.ResetsAt.After(restriction.ObservedAt.Add(claudeThrottleHold)) {
		return restriction.ObservedAt.Add(claudeThrottleHold)
	}
	return restriction.ResetsAt
}

// claudeWindowClaim is Anthropic's representative claim for a usage window.
// Claude Code names the limit it reports from it: seven_day is its "weekly
// limit" and seven_day_overage_included its "Fable limit".
func claudeWindowClaim(window ReportedWindow) string {
	switch {
	case window.DurationSeconds != 7*86400:
		return "five_hour"
	case ClaudeModelFamily(window.ModelName) == "opus":
		return "seven_day_opus"
	case ClaudeModelFamily(window.ModelName) == "sonnet":
		return "seven_day_sonnet"
	case ClaudeModelFamily(window.ModelName) == "fable":
		return "seven_day_overage_included"
	}
	return "seven_day"
}

// claudeWindowAtLimit is whether the window keeps its account from the models
// it covers.
func claudeWindowAtLimit(window ReportedWindow) bool {
	return window.LimitReached || window.UsedPercent >= 100
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
		if (scope == "" || scope == ClaudeModelFamily(model) || scope == canonicalClaudeModelID(model)) && restriction.endsAt().After(blockedUntil) {
			blockedUntil, throttled = restriction.endsAt(), restriction.Claim == ""
		}
	}
	for _, window := range fact.AllowanceSample.Windows {
		if claudeWindowMatchesModel(window, model) && claudeWindowAtLimit(window) && window.ResetsAt.After(blockedUntil) {
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
		resets = append(resets, restriction.endsAt())
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
// for a throttle, which Claude Code retries. A model's weekly window resets
// with the week's, so of the windows that end at reset it names one that holds
// the account back, at its limit or within the account's reserve, when any does.
func claudeLimitClaimEndingAt(fact AccountRoutingFact, model string, reset time.Time) string {
	var ending, holding []ReportedWindow
	for _, window := range fact.AllowanceSample.Windows {
		if !window.ResetsAt.Equal(reset) || !claudeWindowMatchesModel(window, model) {
			continue
		}
		ending = append(ending, window)
		if claudeWindowAtLimit(window) || (fact.Reserve.HardPercent > 0 && window.UsedPercent >= float64(100-fact.Reserve.HardPercent)) {
			holding = append(holding, window)
		}
	}
	if len(holding) > 0 {
		ending = holding
	}
	claim := ""
	for _, window := range ending {
		switch windowClaim := claudeWindowClaim(window); windowClaim {
		case "seven_day_opus", "seven_day_sonnet", "seven_day_overage_included":
			return windowClaim
		case "seven_day":
			claim = windowClaim
		default:
			if claim == "" {
				claim = windowClaim
			}
		}
	}
	if claim != "" {
		return claim
	}
	for _, restriction := range fact.ClaudeRestrictions {
		if restriction.endsAt().Equal(reset) && restriction.Claim != "" {
			return restriction.Claim
		}
	}
	return ""
}

// ClaudeWeeklyAllowance is the free share of the tightest weekly window that
// applies to model, and the earliest reset among those windows. Anthropic's
// weekly windows reset at fixed times, so they always count as started.
func ClaudeWeeklyAllowance(fact AccountRoutingFact, model string, now time.Time) (AllowanceWindow, bool) {
	weekly, known := AllowanceWindow{FreePercent: 100}, false
	for _, window := range fact.AllowanceSample.Windows {
		if window.DurationSeconds != 7*86400 || !window.ResetsAt.After(now) || !claudeWindowMatchesModel(window, model) {
			continue
		}
		weekly.FreePercent = math.Min(weekly.FreePercent, math.Max(0, 100-window.UsedPercent))
		if !known || window.ResetsAt.Before(weekly.ResetsAt) {
			weekly.ResetsAt = window.ResetsAt
		}
		known = true
	}
	return weekly, known
}

func updateClaudeSharedState(fact *AccountRoutingFact, now time.Time) {
	dropEndedClaudeWindows(&fact.AllowanceSample, now)
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

// dropEndedClaudeWindows removes windows whose reset has passed. Anthropic
// stops reporting a five-hour window once it ends and reports a new one only
// after the next request; until then the old reading describes nothing.
func dropEndedClaudeWindows(sample *AllowanceSample, now time.Time) {
	sample.Windows = slices.DeleteFunc(slices.Clone(sample.Windows), func(window ReportedWindow) bool {
		return !window.ResetsAt.After(now)
	})
}
