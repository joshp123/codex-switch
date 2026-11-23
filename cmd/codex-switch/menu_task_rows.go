package main

import (
	"cmp"
	"slices"
	"strings"

	"github.com/joshp123/codex-switch/internal/menupb"
)

// menuTaskRows is how many task rows the menu shows for a period.
const menuTaskRows = 10

// keepShownTaskRows replaces each period's tasks with the rows the menu shows:
// named tasks grouped by provider and the first line of their name, keeping
// the menuTaskRows largest by tokens and the menuTaskRows largest by cost. The
// snapshot then carries tens of rows instead of every task in 30 days.
func keepShownTaskRows(snapshot *menupb.MenuSnapshot) {
	providers := map[string]menupb.Provider{}
	for _, account := range snapshot.Accounts {
		providers[account.Key] = account.Provider
	}
	for _, period := range snapshot.ReportingPeriods {
		if period.TaskUsage != nil {
			period.TaskUsage = &menupb.TaskUsageReport{Tasks: shownTaskRows(period.TaskUsage.Tasks, providers)}
		}
	}
}

type menuTaskRow struct {
	task      *menupb.CompletedTask
	tokens    int64
	cost      int64
	costKnown bool
	accounts  []string
}

func shownTaskRows(tasks []*menupb.CompletedTask, providers map[string]menupb.Provider) []*menupb.CompletedTask {
	rows := map[string]*menuTaskRow{}
	for _, task := range tasks {
		title := firstLine(task.GetTitle())
		var accounts []string
		for _, key := range task.AccountKeys {
			if _, inPool := providers[key]; inPool {
				accounts = append(accounts, key)
			}
		}
		if title == "" || len(accounts) == 0 {
			continue
		}
		id := providers[accounts[0]].String() + " " + title
		row := rows[id]
		if row == nil {
			row = &menuTaskRow{
				task:      &menupb.CompletedTask{TaskId: task.TaskId, Title: &title, LastCompletedAt: task.LastCompletedAt},
				costKnown: true,
			}
			rows[id] = row
		}
		row.tokens += task.GetTokens()
		if task.CostNanoUsd == nil {
			row.costKnown = false
		} else if row.costKnown {
			row.cost += *task.CostNanoUsd
		}
		for _, key := range accounts {
			if !slices.Contains(row.accounts, key) {
				row.accounts = append(row.accounts, key)
			}
		}
		if task.GetLastCompletedAt().AsTime().After(row.task.GetLastCompletedAt().AsTime()) {
			row.task.TaskId, row.task.LastCompletedAt = task.TaskId, task.LastCompletedAt
		}
	}
	all := make([]*menuTaskRow, 0, len(rows))
	for _, row := range rows {
		all = append(all, row)
	}
	largest := func(amount func(*menuTaskRow) int64) []*menuTaskRow {
		slices.SortFunc(all, func(a, b *menuTaskRow) int {
			if order := cmp.Compare(amount(b), amount(a)); order != 0 {
				return order
			}
			return b.task.GetLastCompletedAt().AsTime().Compare(a.task.GetLastCompletedAt().AsTime())
		})
		return slices.Clone(all[:min(menuTaskRows, len(all))])
	}
	var shown []*menupb.CompletedTask
	for _, row := range append(largest(func(row *menuTaskRow) int64 { return row.tokens }), largest(func(row *menuTaskRow) int64 {
		if !row.costKnown {
			return -1
		}
		return row.cost
	})...) {
		if slices.Contains(shown, row.task) {
			continue
		}
		tokens := row.tokens
		slices.Sort(row.accounts)
		row.task.Tokens, row.task.AccountKeys = &tokens, row.accounts
		if row.costKnown {
			cost := row.cost
			row.task.CostNanoUsd = &cost
		} else {
			row.task.CostNanoUsd = nil
		}
		shown = append(shown, row.task)
	}
	return shown
}

// firstLine is a task name's first non-empty line, as the menu shows it.
func firstLine(title string) string {
	lines := strings.FieldsFunc(title, func(r rune) bool { return strings.ContainsRune(lineBreaks, r) })
	if len(lines) == 0 {
		return ""
	}
	return lines[0]
}

// lineBreaks are the characters Swift counts as new lines.
const lineBreaks = "\n\r\v\f\u0085\u2028\u2029"
