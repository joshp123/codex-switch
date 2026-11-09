package accounts

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/joshp123/codex-switch/internal/logtime"
)

// ResetKind is what a stored reset gives back when it is spent.
type ResetKind string

const (
	// ResetWeekly gives back the weekly allowance for every model.
	ResetWeekly ResetKind = "weekly"
	// ResetFiveHour gives back the five-hour allowance.
	ResetFiveHour ResetKind = "five-hour"
	// ResetFull gives back the five-hour and weekly allowances.
	ResetFull ResetKind = "full"
	// ResetFable gives back what Fable models used of the week: the Fable
	// allowance is full again and the weekly allowance regains that share.
	// The week keeps its reset time.
	ResetFable ResetKind = "Fable"
)

// StoredReset is the resets of one kind that an account holds and that expire
// together.
type StoredReset struct {
	Kind  ResetKind
	Count uint32
	// ExpiresAt is zero when the provider states no expiry.
	ExpiresAt time.Time
}

// HeldResets is the resets an account holds at now: how many, and the list
// of them, soonest expiry first, when the provider's list is known. The count
// is nil when the provider has not said.
//
// Anthropic gives a Claude account's list and nothing else, so the count is
// the list's. OpenAI gives a Codex account's count with each usage reading,
// and its list of reset credits is read apart, every five minutes. While the
// two disagree the list is the older, and only the count is given.
func HeldResets(fact AccountRoutingFact, now time.Time) (count *uint32, list []StoredReset) {
	var held []StoredReset
	if fact.Provider == ProviderClaude {
		if fact.AllowanceSample.StoredResetsReadAt.IsZero() {
			return nil, nil
		}
		for _, reset := range fact.AllowanceSample.StoredResets {
			if reset.ExpiresAt.IsZero() || reset.ExpiresAt.After(now) {
				held = append(held, reset)
			}
		}
	} else {
		if fact.ResetCreditsReadAt.IsZero() {
			return fact.AllowanceSample.AvailableResetCredits, nil
		}
		for _, credit := range fact.ResetCredits {
			if credit.usable(now) {
				held = append(held, StoredReset{Kind: ResetWeekly, Count: 1, ExpiresAt: credit.ExpiresAt})
			}
		}
	}
	list = groupStoredResets(held)
	var listed uint32
	for _, reset := range list {
		listed += reset.Count
	}
	if reported := fact.AllowanceSample.AvailableResetCredits; fact.Provider != ProviderClaude && reported != nil && *reported != listed {
		return reported, nil
	}
	return &listed, list
}

// groupStoredResets counts resets of one kind that expire together as one
// entry, and orders the entries by expiry, those that never expire last.
func groupStoredResets(resets []StoredReset) []StoredReset {
	var grouped []StoredReset
	for _, reset := range resets {
		if reset.Count == 0 {
			continue
		}
		index := slices.IndexFunc(grouped, func(other StoredReset) bool {
			return other.Kind == reset.Kind && other.ExpiresAt.Equal(reset.ExpiresAt)
		})
		if index < 0 {
			grouped = append(grouped, reset)
		} else {
			grouped[index].Count += reset.Count
		}
	}
	slices.SortStableFunc(grouped, func(left, right StoredReset) int {
		switch {
		case left.ExpiresAt.Equal(right.ExpiresAt):
			return strings.Compare(string(left.Kind), string(right.Kind))
		case left.ExpiresAt.IsZero():
			return 1
		case right.ExpiresAt.IsZero():
			return -1
		}
		return left.ExpiresAt.Compare(right.ExpiresAt)
	})
	return grouped
}

// StoredResetLabel names resets the way status, the proxy log and the menu do:
// "1 weekly reset", "3 Fable resets", or "2 resets" for a kind the provider
// names in no way this build knows.
func StoredResetLabel(kind ResetKind, count uint32) string {
	noun := "resets"
	if count == 1 {
		noun = "reset"
	}
	if kind == "" {
		return fmt.Sprintf("%d %s", count, noun)
	}
	return fmt.Sprintf("%d %s %s", count, kind, noun)
}

// describeStoredResets is the account's stored resets for the proxy log: each
// kind with its expiry, or the provider's reason for listing none. It is ""
// for an account that holds none and was given no reason.
func describeStoredResets(fact AccountRoutingFact, now time.Time) string {
	if reason := fact.AllowanceSample.NoResetsReason; fact.Provider == ProviderClaude && reason != "" {
		return fmt.Sprintf("Anthropic lists it no stored resets (%s)", reason)
	}
	count, list := HeldResets(fact, now)
	var held []string
	for _, reset := range list {
		label := StoredResetLabel(reset.Kind, reset.Count)
		if !reset.ExpiresAt.IsZero() {
			verb := "expire"
			if reset.Count == 1 {
				verb = "expires"
			}
			label += fmt.Sprintf(", %s %s", verb, logtime.Format(reset.ExpiresAt))
		}
		held = append(held, label)
	}
	if len(held) == 0 && count != nil && *count > 0 {
		held = []string{StoredResetLabel("", *count)}
	}
	if len(held) == 0 {
		return ""
	}
	return "holds " + strings.Join(held, " and ")
}
