package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"

	accountapi "github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/menupb"
	"google.golang.org/protobuf/proto"
)

func status(args []string) {
	flags := flag.NewFlagSet("status", flag.ExitOnError)
	_ = flags.Parse(args)
	if flags.NArg() != 0 {
		log.Fatal("usage: codex-switch [status]")
	}
	snapshot, err := fetchMenuSnapshot(dataDir())
	if err != nil {
		log.Fatalf("cannot read current Codex Switch status: %v", err)
	}
	writeDeploymentStatus(os.Stdout, dataDir())
	writeCurrentStatus(os.Stdout, snapshot)
	fmt.Printf("\nProxy log: %s\n", proxyLogPath(dataDir()))
}

func writeDeploymentStatus(output io.Writer, dataDirectory string) {
	published, err := os.ReadFile(filepath.Join(dataDirectory, menuSnapshotURLFilename))
	if err == nil {
		u, parseErr := url.Parse(strings.TrimSpace(string(published)))
		if parseErr == nil && u.Scheme == "http" && validateLocalListenAddress(u.Host) == nil {
			u.Path = strings.TrimSuffix(u.Path, menuSnapshotEndpoint) + deployStatusEndpoint
			u.RawQuery = ""
			client := http.Client{Timeout: 5 * time.Second}
			if response, err := client.Get(u.String()); err == nil {
				var status deployStatusResponse
				if response.StatusCode == http.StatusOK && json.NewDecoder(io.LimitReader(response.Body, 4096)).Decode(&status) == nil {
					fmt.Fprintf(output, "Proxy build: %d\n", status.Build)
				}
				response.Body.Close()
			}
		}
	}
	if skipped, err := os.ReadFile(filepath.Join(dataDirectory, "skipped-builds")); err == nil && strings.TrimSpace(string(skipped)) != "" {
		fmt.Fprintf(output, "Skipped builds: %s\n", strings.Join(strings.Fields(string(skipped)), ", "))
	}
}

// fetchMenuSnapshot reads the snapshot the serving proxy last composed.
func fetchMenuSnapshot(dataDirectory string) (*menupb.MenuSnapshot, error) {
	published, err := os.ReadFile(filepath.Join(dataDirectory, menuSnapshotURLFilename))
	if err != nil {
		return nil, fmt.Errorf("no proxy has served from %s: %w", dataDirectory, err)
	}
	snapshotURL, err := url.Parse(strings.TrimSpace(string(published)))
	if err != nil || snapshotURL.Scheme != "http" {
		return nil, fmt.Errorf("%s does not hold a local proxy URL", menuSnapshotURLFilename)
	}
	if err := validateLocalListenAddress(snapshotURL.Host); err != nil {
		return nil, err
	}
	client := http.Client{Timeout: 5 * time.Second}
	response, err := client.Get(snapshotURL.String())
	if err != nil {
		return nil, fmt.Errorf("the proxy at %s did not answer", snapshotURL.Host)
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK:
	case http.StatusServiceUnavailable:
		return nil, errors.New("the proxy is composing its first snapshot; try again in a minute")
	default:
		return nil, fmt.Errorf("the proxy answered HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	snapshot := &menupb.MenuSnapshot{}
	if err := proto.Unmarshal(body, snapshot); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func writeCurrentStatus(output io.Writer, snapshot *menupb.MenuSnapshot) {
	pool := make([]accountapi.PoolAccount, 0, len(snapshot.Accounts))
	for _, account := range snapshot.Accounts {
		provider := accountapi.ProviderCodex
		if account.Provider == menupb.Provider_PROVIDER_CLAUDE {
			provider = accountapi.ProviderClaude
		}
		pool = append(pool, accountapi.PoolAccount{Name: account.Key, Provider: provider})
	}
	accountName := func(key string) string {
		name, _ := accountapi.AccountDisplayName(pool, key)
		return name
	}
	accountNames := func(keys []string) string {
		names := make([]string, 0, len(keys))
		for _, key := range keys {
			names = append(names, accountName(key))
		}
		return strings.Join(names, ", ")
	}
	available := 0
	hasChatGPTAccount := false
	for _, account := range snapshot.Accounts {
		hasChatGPTAccount = hasChatGPTAccount || account.Provider != menupb.Provider_PROVIDER_CLAUDE
		if account.Usable {
			available++
		}
	}
	fmt.Fprintf(output, "%d of %d accounts can take new work.\n", available, len(snapshot.Accounts))
	for _, burn := range snapshot.ProviderBurns {
		providerName := "Codex"
		if burn.Provider == menupb.Provider_PROVIDER_CLAUDE {
			providerName = "Claude"
		}
		poolSize := 0
		for _, account := range snapshot.Accounts {
			if account.Provider == burn.Provider && account.OffersConfiguredModel {
				poolSize++
			}
		}
		fmt.Fprintf(output, "%s: burning %.1f accounts a week, %d in the pool\n", providerName, burn.AccountsPerWeek, poolSize)
	}
	if hasChatGPTAccount && !snapshot.GetConnection().GetNewTasksConfigured() {
		fmt.Fprintln(output, "Codex is not configured to send new work to this proxy.")
	}
	for _, account := range snapshot.Accounts {
		state := "available"
		if !account.Usable {
			state = "unavailable: " + oneLine(account.UnavailableReason)
		} else if account.ReserveStatus != "" {
			state = oneLine(account.ReserveStatus)
		}
		label := accountName(account.Key)
		for _, detail := range []string{account.Email, account.Plan} {
			if detail != "" {
				label += " · " + oneLine(detail)
			}
		}
		fmt.Fprintf(output, "\n%s · %s\n", label, state)
		for _, window := range account.GetAllowance().GetWindows() {
			label := accountapi.AllowancePeriodLabel(window.PeriodSeconds)
			model := window.ModelName
			if model == "" {
				model = window.ModelId
			}
			if model != "" {
				label += " · " + oneLine(model)
			}
			fmt.Fprintf(output, "  %-8s %.0f%% used · %.0f%% free", label, window.UsedPercent, 100-window.UsedPercent)
			if window.ResetAt != nil {
				fmt.Fprintf(output, " · resets %s", window.ResetAt.AsTime().Local().Format("Mon 2 Jan 15:04"))
			}
			fmt.Fprintln(output)
		}
	}
	if activity := snapshot.ProxyActivity; activity != nil {
		writeProxyActivity(output, activity, accountName, accountNames)
	}
	if len(snapshot.ReportingPeriods) > 0 {
		writeAccountTokenTable(output, snapshot, accountName)
	}
	var tasks []*menupb.CompletedTask
	heading := ""
	for _, period := range snapshot.ReportingPeriods {
		if period.Kind == menupb.ReportingPeriodKind_REPORTING_PERIOD_TODAY && period.TaskUsage != nil {
			tasks = slices.Clone(period.TaskUsage.Tasks)
			slices.SortFunc(tasks, func(a, b *menupb.CompletedTask) int {
				if a.GetTokens() > b.GetTokens() {
					return -1
				}
				if a.GetTokens() < b.GetTokens() {
					return 1
				}
				return 0
			})
			tasks = slices.DeleteFunc(tasks, func(task *menupb.CompletedTask) bool { return task.GetTitle() == "" })
			if len(tasks) > 10 {
				tasks = tasks[:10]
			}
			heading = "Top tasks on this Mac today:"
		}
	}
	if len(tasks) > 0 {
		fmt.Fprintln(output, "\n"+heading)
		for _, task := range tasks {
			fmt.Fprintf(output, "  %s", taskLabel(task))
			if task.Tokens != nil {
				fmt.Fprintf(output, " · %s tokens", groupedTokens(task.GetTokens()))
			}
			if task.CostNanoUsd != nil {
				fmt.Fprintf(output, " · %s", dollars(task.GetCostNanoUsd()))
			}
			if len(task.AccountKeys) > 0 {
				fmt.Fprintf(output, " · %s", accountNames(task.AccountKeys))
			}
			fmt.Fprintln(output)
		}
	}
}

func writeProxyActivity(output io.Writer, activity *menupb.ProxyActivity, accountName func(string) string, accountNames func([]string) string) {
	if activity.RequestsStartedAt != nil {
		fmt.Fprintf(output, "\nModel requests through this proxy · since %s\n", activity.RequestsStartedAt.AsTime().Local().Format("2 Jan 2006 15:04 MST"))
		writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
		fmt.Fprintln(writer, "Account\tRequests\tSuccessful\tFailed\tInterrupted")
		for _, account := range activity.Accounts {
			counts := account.GetRequestCounts()
			fmt.Fprintf(writer, "%s\t%s\t%s\t%s\t%s\n", accountName(account.AccountKey), groupedTokens(counts.GetRequests()), groupedTokens(counts.GetSuccessful()), groupedTokens(counts.GetFailed()), groupedTokens(counts.GetInterrupted()))
		}
		_ = writer.Flush()
	}
	fmt.Fprintf(output, "\nThrough this proxy · %s–%s\n", activity.GetStartedAt().AsTime().Local().Format("Mon 2 Jan 15:04"), activity.GetEndedAt().AsTime().Local().Format("15:04"))
	writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
	fmt.Fprintln(writer, "Account\tCompleted replies\tTokens")
	var replies, tokens int64
	for _, account := range activity.Accounts {
		fmt.Fprintf(writer, "%s\t%s\t%s\n", accountName(account.AccountKey), groupedTokens(account.CompletedResponses), groupedTokens(account.Tokens))
		replies += account.CompletedResponses
		tokens += account.Tokens
	}
	fmt.Fprintf(writer, "Total\t%s\t%s\n", groupedTokens(replies), groupedTokens(tokens))
	_ = writer.Flush()
	if len(activity.Tasks) > 0 {
		fmt.Fprintln(output, "\nRecent routed tasks in this period:")
		for _, task := range activity.Tasks {
			fmt.Fprintf(output, "  %s · %s tokens · %s\n", taskLabel(task), groupedTokens(task.GetTokens()), accountNames(task.AccountKeys))
		}
	}
}

func writeAccountTokenTable(output io.Writer, snapshot *menupb.MenuSnapshot, accountName func(string) string) {
	fmt.Fprintln(output, "\nAccount usage · UTC days")
	periods := []menupb.ReportingPeriodKind{menupb.ReportingPeriodKind_REPORTING_PERIOD_TODAY, menupb.ReportingPeriodKind_REPORTING_PERIOD_SEVEN_DAYS, menupb.ReportingPeriodKind_REPORTING_PERIOD_THIRTY_DAYS}
	byPeriod := make(map[menupb.ReportingPeriodKind]map[string]*menupb.AccountTotal)
	for _, period := range snapshot.ReportingPeriods {
		byPeriod[period.Kind] = make(map[string]*menupb.AccountTotal)
		for _, account := range period.Accounts {
			byPeriod[period.Kind][account.AccountKey] = account
		}
	}
	for _, table := range []struct {
		heading string
		value   func(*menupb.AccountTotal) int64
		format  func(int64) string
	}{
		{"Tokens", (*menupb.AccountTotal).GetTokens, groupedTokens},
		{"API-equivalent cost", (*menupb.AccountTotal).GetCostNanoUsd, dollars},
	} {
		writer := tabwriter.NewWriter(output, 0, 4, 2, ' ', 0)
		fmt.Fprintf(writer, "\n%s\tToday\t7 Days\t30 Days\n", table.heading)
		totals := make(map[menupb.ReportingPeriodKind]int64)
		for _, account := range snapshot.Accounts {
			fmt.Fprint(writer, accountName(account.Key))
			for _, kind := range periods {
				value := table.value(byPeriod[kind][account.Key])
				totals[kind] += value
				fmt.Fprintf(writer, "\t%s", table.format(value))
			}
			fmt.Fprintln(writer)
		}
		fmt.Fprint(writer, "Total")
		for _, kind := range periods {
			fmt.Fprintf(writer, "\t%s", table.format(totals[kind]))
		}
		fmt.Fprintln(writer)
		_ = writer.Flush()
	}
}

func dollars(nanoUSD int64) string {
	cents := (nanoUSD + 5_000_000) / 10_000_000
	return "$" + groupedTokens(cents/100) + fmt.Sprintf(".%02d", cents%100)
}

func groupedTokens(value int64) string {
	digits := strconv.FormatInt(value, 10)
	for index := len(digits) - 3; index > 0; index -= 3 {
		digits = digits[:index] + "," + digits[index:]
	}
	return digits
}

// taskLabel names a task by its title, shortened to one line, or by its ID.
func taskLabel(task *menupb.CompletedTask) string {
	if task.TaskId == "" {
		return "Requests without a task ID"
	}
	label := []rune(oneLine(task.GetTitle()))
	if len(label) == 0 {
		return task.TaskId
	}
	if len(label) > 72 {
		return string(label[:71]) + "…"
	}
	return string(label)
}

func oneLine(value string) string {
	value = strings.Map(func(char rune) rune {
		if unicode.IsControl(char) {
			return ' '
		}
		return char
	}, value)
	return strings.Join(strings.Fields(value), " ")
}
