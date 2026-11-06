package usage

import (
	"fmt"
	"iter"
	"slices"
	"strings"
	"time"

	"github.com/joshp123/codex-switch/internal/store"
)

type taskMetadataLookup func(string) taskMetadata

type taskTotal struct {
	TaskID          string
	Title           string
	Tokens          int64
	Cost            *nanoUSD
	Accounts        []store.AccountKey
	LastCompletedAt time.Time
}

// Resolve lineage when reading totals, so a late parent, fork, or turn owner
// moves all of its existing cohorts without rewriting their contributions.
func taskOrigin(taskID, rootTurnID string, byTask taskMetadataLookup, turnOwner func(string) string) (owner string, system bool, err error) {
	owner = taskID
	unresolvedOrigin := false
	if rootTurnID != "" && turnOwner != nil {
		if origin := turnOwner(rootTurnID); origin != "" {
			owner = origin
		} else {
			unresolvedOrigin = true
		}
	}
	visited := make(map[string]bool)
	for {
		if visited[owner] {
			return "", false, fmt.Errorf("cyclic task parent relationship at %s", owner)
		}
		visited[owner] = true
		parent := byTask(owner).ParentTaskID
		if parent == "" {
			parent = byTask(owner).ForkedFromTaskID
		}
		if parent == "" {
			break
		}
		owner = parent
	}
	system = owner == "" || (!unresolvedOrigin && strings.HasPrefix(byTask(owner).SourceKind, "subagent:") && byTask(owner).ParentTaskID == "")
	if system {
		owner = "system"
	}
	return owner, system, nil
}

func totalTaskCohorts(cohorts iter.Seq2[usageCohortKey, usageCohortAmount], metadata taskMetadataLookup, turnOwner func(string) string, start, end time.Time) ([]taskTotal, error) {
	periods, err := totalTaskCohortPeriods(cohorts, metadata, turnOwner, []time.Time{start}, end)
	if err != nil {
		return nil, err
	}
	return periods[0], nil
}

// Resolve each cohort's origin once, then add it to every period containing
// its date. Periods share the same accumulation rules as single-period activity.
func totalTaskCohortPeriods(cohorts iter.Seq2[usageCohortKey, usageCohortAmount], metadata taskMetadataLookup, turnOwner func(string) string, starts []time.Time, end time.Time) ([][]taskTotal, error) {
	type periodTotals struct {
		tasks        map[string]*taskTotal
		unattributed map[string]bool
	}
	periods := make([]periodTotals, len(starts))
	oldest := end
	for index, start := range starts {
		periods[index] = periodTotals{tasks: make(map[string]*taskTotal), unattributed: make(map[string]bool)}
		if start.Before(oldest) {
			oldest = start
		}
	}
	for key, amount := range cohorts {
		if amount.Count <= 0 || key.Date.Before(oldest) || !key.Date.Before(end) {
			continue
		}
		owner, system, err := taskOrigin(key.TaskID, key.RootTurnID, metadata, turnOwner)
		if err != nil {
			return nil, err
		}
		for index, start := range starts {
			if key.Date.Before(start) {
				continue
			}
			period := &periods[index]
			total := period.tasks[owner]
			if total == nil {
				zero := nanoUSD(0)
				total = &taskTotal{TaskID: owner, Title: localTaskTitle(owner, system, metadata(owner)), Cost: &zero}
				period.tasks[owner] = total
			}
			total.Tokens += amount.Tokens
			if amount.Unattributed > 0 {
				period.unattributed[owner] = true
			}
			if amount.UnknownPrices > 0 {
				total.Cost = nil
			} else if total.Cost != nil {
				*total.Cost += amount.Cost
			}
			if key.Account != "" && !slices.Contains(total.Accounts, key.Account) {
				total.Accounts = append(total.Accounts, key.Account)
			}
			if amount.LastCompletedAt.After(total.LastCompletedAt) {
				total.LastCompletedAt = amount.LastCompletedAt
			}
		}
	}
	result := make([][]taskTotal, len(periods))
	for index, period := range periods {
		result[index] = finishTaskTotals(period.tasks, period.unattributed)
	}
	return result, nil
}

func finishTaskTotals(totals map[string]*taskTotal, tasksWithUnattributedUsage map[string]bool) []taskTotal {
	result := make([]taskTotal, 0, len(totals))
	for _, total := range totals {
		if tasksWithUnattributedUsage[total.TaskID] {
			total.Accounts = nil
		}
		slices.Sort(total.Accounts)
		result = append(result, *total)
	}
	slices.SortFunc(result, func(a, b taskTotal) int { return b.LastCompletedAt.Compare(a.LastCompletedAt) })
	return result
}
