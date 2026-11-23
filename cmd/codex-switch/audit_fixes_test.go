package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/joshp123/codex-switch/internal/menupb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestKeepShownTaskRowsPreservesUnknownCost(t *testing.T) {
	tokens := int64(1000)
	title := "Unnamed model task"
	snapshot := &menupb.MenuSnapshot{
		Accounts: []*menupb.Account{
			{Key: "chatgpt:a@example.com", Provider: menupb.Provider_PROVIDER_CHATGPT, AccountNumber: 1},
		},
		ReportingPeriods: []*menupb.ReportingPeriod{
			{
				Kind: menupb.ReportingPeriodKind_REPORTING_PERIOD_TODAY,
				TaskUsage: &menupb.TaskUsageReport{
					Tasks: []*menupb.CompletedTask{
						{
							TaskId:          "task-1",
							Title:           &title,
							AccountKeys:     []string{"chatgpt:a@example.com"},
							Tokens:          &tokens,
							CostNanoUsd:     nil,
							LastCompletedAt: timestamppb.New(time.Now()),
						},
					},
				},
			},
		},
	}
	keepShownTaskRows(snapshot)
	got := snapshot.ReportingPeriods[0].TaskUsage.Tasks
	if len(got) != 1 {
		t.Fatalf("got %d tasks, want 1", len(got))
	}
	if got[0].CostNanoUsd != nil {
		t.Fatalf("expected nil CostNanoUsd when cost is unknown, got %d", *got[0].CostNanoUsd)
	}
}

func TestDollarsAndGroupedTokensHandleNegatives(t *testing.T) {
	if got := groupedTokens(-1234567); got != "-1,234,567" {
		t.Fatalf("groupedTokens(-1234567) = %q", got)
	}
	if got := dollars(-12_340_000_000); got != "-$12.34" {
		t.Fatalf("dollars(-12.34) = %q", got)
	}
}

func TestWriteCurrentStatusHandlesGeminiUsageOnly(t *testing.T) {
	now := time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	snapshot := &menupb.MenuSnapshot{
		GeneratedAt: timestamppb.New(now),
		Connection:  &menupb.Connection{NewTasksConfigured: true},
		Accounts: []*menupb.Account{
			{
				Key: "chatgpt:a@example.com", AccountNumber: 1, Email: "a@example.com", Plan: "Pro",
				Provider: menupb.Provider_PROVIDER_CHATGPT, State: menupb.AccountState_ACCOUNT_STATE_READY,
				Usable: true, OffersConfiguredModel: true,
			},
			{
				Key: "gemini:g@example.com", AccountNumber: 1, Email: "g@example.com", Plan: "Antigravity",
				Provider: menupb.Provider_PROVIDER_GEMINI, State: menupb.AccountState_ACCOUNT_STATE_READY,
				Usable: false, UnavailableReason: "usage only", OffersConfiguredModel: true,
				Allowance: &menupb.AllowanceObservation{
					ObservedAt: timestamppb.New(now),
					Allowed:    true,
					Windows: []*menupb.AllowanceWindow{
						{PeriodSeconds: 5 * 3600, UsedPercent: 7, ResetAt: timestamppb.New(now.Add(2 * time.Hour))},
						{PeriodSeconds: 7 * 86400, UsedPercent: 30, ResetAt: timestamppb.New(now.Add(5 * 24 * time.Hour))},
						{PeriodSeconds: 7 * 86400, UsedPercent: 6, ResetAt: timestamppb.New(now.Add(6 * 24 * time.Hour)), ModelId: "3p", ModelName: "Claude & GPT"},
					},
				},
			},
		},
	}
	var out bytes.Buffer
	writeCurrentStatus(&out, snapshot)
	text := out.String()
	if !strings.Contains(text, "1 of 1 accounts can take new work.") {
		t.Fatalf("expected routed pool count 1 of 1, got:\n%s", text)
	}
	if !strings.Contains(text, "Gemini 1 · g@example.com · Antigravity · usage only") {
		t.Fatalf("expected Gemini usage-only header, got:\n%s", text)
	}
	if !strings.Contains(text, "weekly · Claude & GPT") {
		t.Fatalf("expected 3p window line, got:\n%s", text)
	}
}
