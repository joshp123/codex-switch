package main

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	accountapi "github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/clientcapability"
	"github.com/joshp123/codex-switch/internal/codexstate"
	"github.com/joshp123/codex-switch/internal/menupb"
	"github.com/joshp123/codex-switch/internal/relay"
	"github.com/joshp123/codex-switch/internal/routing"
	"github.com/joshp123/codex-switch/internal/store"
	"github.com/joshp123/codex-switch/internal/usage"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const menuSnapshotEndpoint = "/codex-switch/menu-snapshot"

// menuSnapshotURLFilename is the file in the data directory that holds the
// serving proxy's private menu snapshot URL. The menu app and the status
// command read it; each generation writes it once it listens.
const menuSnapshotURLFilename = "menu-snapshot-url"

// The snapshot is composed once a minute, and menuSnapshotAfterWork after the
// proxy finishes a response when the last snapshot is older than that, so
// work after a quiet spell shows within seconds while continuous work costs
// no more than one snapshot a minute.
const (
	menuSnapshotInterval     = time.Minute
	menuSnapshotAfterWork    = 5 * time.Second
	menuSnapshotWorkInterval = time.Minute
	// A menu waiting for a newer snapshot is answered within this, so it learns
	// the snapshot is still current.
	menuSnapshotLongestWait = 55 * time.Second
)

// menuSnapshots composes the menu snapshot and serves the latest one, so a
// reader never waits for accounts, usage or task names. A reader that already
// has the latest snapshot waits until it changes.
type menuSnapshots struct {
	dataDirectory string
	// codexBaseURL is the openai_base_url that sends Codex work to this proxy.
	codexBaseURL string
	router       *routing.TaskRouter
	database     *store.StoreDatabase
	usageLedger  *usage.UsageLedger
	codex        *codexstate.LocalReader

	mutex   sync.Mutex
	encoded []byte
	// content is the latest snapshot without the time it was composed, and
	// version counts changes to it. changed is closed and replaced at each
	// change, waking every waiting reader; stopping wakes them when this
	// generation stops serving.
	content []byte
	// version is unique to this process, so a menu that held a snapshot from
	// the previous generation never takes this one's for the same version.
	generation int64
	version    uint64
	changed    chan struct{}
	stopping   chan struct{}
	// workDone asks for a snapshot soon, because the proxy finished a response.
	workDone chan struct{}
	// lastCodexConfig is Codex's effective config as last read, and
	// lastCodexConfigInputs the identity of the files it was read from. Codex
	// is asked again only when those files change. While Codex cannot answer,
	// for example in the first seconds after the Mac wakes, the last config
	// stands in so the rest of the menu stays current.
	lastCodexConfig       *codexstate.EffectiveModelConfig
	lastCodexConfigInputs string
	codexTitles           codexTaskTitles
}

// codexTaskTitles holds the names Codex gave tasks, read from its thread
// database while that database is unchanged.
type codexTaskTitles struct {
	inputs string
	titles map[string]string
	// unnamed lists tasks Codex has no record of, so they are not asked for
	// again until the database changes.
	unnamed map[string]bool
}

func newMenuSnapshots(dataDirectory string, router *routing.TaskRouter, database *store.StoreDatabase, usageLedger *usage.UsageLedger, codex *codexstate.LocalReader) *menuSnapshots {
	return &menuSnapshots{
		dataDirectory: dataDirectory, router: router, database: database, usageLedger: usageLedger, codex: codex,
		generation: time.Now().UnixNano(),
		changed:    make(chan struct{}), stopping: make(chan struct{}), workDone: make(chan struct{}, 1),
	}
}

// composeAfterCompletedResponse records each completed response and asks for
// the menu to show it soon.
type composeAfterCompletedResponse struct {
	ledger    relay.CompletionLedger
	snapshots *menuSnapshots
	activity  *deploymentActivity
}

func (recorder composeAfterCompletedResponse) RecordCompletedResponse(record store.CompletedResponseRecord) error {
	if recorder.activity != nil {
		recorder.activity.lastCompleted.Store(time.Now().UnixNano())
	}
	if err := recorder.ledger.RecordCompletedResponse(record); err != nil {
		return err
	}
	select {
	case recorder.snapshots.workDone <- struct{}{}:
	default:
	}
	return nil
}

// StopWaiting answers every waiting reader at once, so a stopping generation
// holds no request open and the menu moves to the next generation.
func (snapshots *menuSnapshots) StopWaiting() {
	snapshots.mutex.Lock()
	defer snapshots.mutex.Unlock()
	select {
	case <-snapshots.stopping:
	default:
		close(snapshots.stopping)
	}
}

func (snapshots *menuSnapshots) Run(ctx context.Context) {
	for {
		wait := time.After(menuSnapshotInterval)
		started := time.Now()
		err := snapshots.compose(ctx)
		if took := time.Since(started); took > 10*time.Second {
			log.Printf("menu snapshot took %s to compose", took.Round(time.Second))
		}
		if err != nil && ctx.Err() == nil {
			log.Printf("could not compose the menu snapshot: %v", err)
			// Try again soon rather than leaving the menu a minute behind.
			wait = time.After(5 * time.Second)
		}
		select {
		case <-ctx.Done():
			return
		case <-wait:
		case <-snapshots.workDone:
			delay := max(menuSnapshotAfterWork, time.Until(started.Add(menuSnapshotWorkInterval)))
			select {
			case <-ctx.Done():
				return
			case <-time.After(delay):
			}
		}
	}
}

func (snapshots *menuSnapshots) compose(ctx context.Context) error {
	installed, err := accountapi.ReadPool(snapshots.dataDirectory)
	if err != nil {
		return err
	}
	hasChatGPTAccounts := slices.ContainsFunc(installed, func(account accountapi.PoolAccount) bool {
		return account.Provider != accountapi.ProviderClaude
	})
	var config codexstate.EffectiveModelConfig
	if hasChatGPTAccounts {
		if config, err = snapshots.codexModelConfig(ctx); err != nil {
			return err
		}
	}
	snapshot, err := readServingMenuSnapshot(snapshots.dataDirectory, snapshots.router, config.Model)
	if err != nil {
		return err
	}
	snapshot.Connection.NewTasksConfigured = config.Provider == "openai" && config.OpenAIBaseURL != nil && strings.TrimRight(*config.OpenAIBaseURL, "/") == snapshots.codexBaseURL
	if err := readProviderBurns(snapshots.database, snapshot); err != nil {
		return err
	}
	var accountKeys []string
	for _, account := range snapshot.Accounts {
		accountKeys = append(accountKeys, account.Key)
	}
	if snapshot.ReportingPeriods, snapshot.AccountDays, snapshot.ProxyActivity, err = snapshots.usageLedger.ReadUsage(ctx, accountKeys); err != nil {
		return err
	}
	if hasChatGPTAccounts {
		if err := snapshots.nameCodexTasks(ctx, snapshot); err != nil {
			return err
		}
	}
	keepShownTaskRows(snapshot)
	deterministic := proto.MarshalOptions{Deterministic: true}
	composedAt := snapshot.GeneratedAt
	snapshot.GeneratedAt = nil
	content, err := deterministic.Marshal(snapshot)
	if err != nil {
		return err
	}
	snapshot.GeneratedAt = composedAt
	encoded, err := deterministic.Marshal(snapshot)
	if err != nil {
		return err
	}
	snapshots.mutex.Lock()
	defer snapshots.mutex.Unlock()
	snapshots.encoded = encoded
	if !bytes.Equal(content, snapshots.content) {
		snapshots.content = content
		snapshots.version++
		close(snapshots.changed)
		snapshots.changed = make(chan struct{})
	}
	return nil
}

func readProviderBurns(database *store.StoreDatabase, snapshot *menupb.MenuSnapshot) error {
	for _, provider := range []menupb.Provider{menupb.Provider_PROVIDER_CHATGPT, menupb.Provider_PROVIDER_CLAUDE} {
		var accountNames []store.AccountName
		for _, account := range snapshot.Accounts {
			if account.Provider == provider {
				accountNames = append(accountNames, store.AccountName(account.Key))
			}
		}
		burn, err := database.ReadWeeklyAllowanceBurn(accountNames, snapshot.GeneratedAt.AsTime())
		if err != nil {
			return fmt.Errorf("read weekly allowance burn: %w", err)
		}
		if burn != nil {
			snapshot.ProviderBurns = append(snapshot.ProviderBurns, &menupb.ProviderBurn{Provider: provider, AccountsPerWeek: *burn})
		}
	}
	return nil
}

// codexModelConfig is Codex's effective model config. It starts a Codex
// app-server to read it only when one of the files Codex reads it from has
// changed since the last read.
func (snapshots *menuSnapshots) codexModelConfig(ctx context.Context) (codexstate.EffectiveModelConfig, error) {
	inputs := snapshots.codex.ModelConfigInputs()
	if snapshots.lastCodexConfig != nil && inputs == snapshots.lastCodexConfigInputs {
		return *snapshots.lastCodexConfig, nil
	}
	configContext, cancel := context.WithTimeout(ctx, 10*time.Second)
	config, err := snapshots.codex.ModelConfig(configContext)
	cancel()
	switch {
	case err == nil:
		snapshots.lastCodexConfig, snapshots.lastCodexConfigInputs = &config, inputs
		return config, nil
	case snapshots.lastCodexConfig != nil:
		log.Printf("menu snapshot uses the last Codex model settings: %v", err)
		return *snapshots.lastCodexConfig, nil
	default:
		return config, err
	}
}

// nameCodexTasks gives each task the name Codex knows it by. A task Codex
// has no record of keeps no name. Codex's database is read only for tasks
// not yet named since it last changed.
func (snapshots *menuSnapshots) nameCodexTasks(ctx context.Context, snapshot *menupb.MenuSnapshot) error {
	tasks := slices.Clone(snapshot.GetProxyActivity().GetTasks())
	for _, period := range snapshot.ReportingPeriods {
		tasks = append(tasks, period.GetTaskUsage().GetTasks()...)
	}
	var ids []string
	for _, task := range tasks {
		if task.Title == nil && task.TaskId != "" && !slices.Contains(ids, task.TaskId) {
			ids = append(ids, task.TaskId)
		}
	}
	inputs, err := snapshots.codex.TaskTitleInputs()
	if err != nil {
		return fmt.Errorf("read Codex task names: %w", err)
	}
	known := &snapshots.codexTitles
	if known.inputs != inputs || known.titles == nil {
		*known = codexTaskTitles{inputs: inputs, titles: map[string]string{}, unnamed: map[string]bool{}}
	}
	var unknown []string
	for _, id := range ids {
		if _, named := known.titles[id]; !named && !known.unnamed[id] {
			unknown = append(unknown, id)
		}
	}
	titles, err := snapshots.codex.TaskTitles(ctx, unknown)
	if err != nil {
		return fmt.Errorf("read Codex task names: %w", err)
	}
	for _, id := range unknown {
		if title, named := titles[id]; named {
			known.titles[id] = title
		} else {
			known.unnamed[id] = true
		}
	}
	for _, task := range tasks {
		if title, ok := known.titles[task.TaskId]; ok && task.Title == nil {
			task.Title = &title
		}
	}
	return nil
}

// serveOrRelay serves the latest snapshot. A reader that passes the version
// it has (?after=N) waits until a newer one exists, up to
// menuSnapshotLongestWait; if none came, it gets 304 Not Modified with the time
// the snapshot was last confirmed current.
func (snapshots *menuSnapshots) serveOrRelay(capability string, next http.Handler) http.Handler {
	path := clientcapability.PathPrefix(capability) + menuSnapshotEndpoint
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != path {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		read := func() ([]byte, string, chan struct{}) {
			snapshots.mutex.Lock()
			defer snapshots.mutex.Unlock()
			return snapshots.encoded, fmt.Sprintf("%d.%d", snapshots.generation, snapshots.version), snapshots.changed
		}
		encoded, version, changed := read()
		if encoded == nil {
			http.Error(w, "the first menu snapshot is not ready", http.StatusServiceUnavailable)
			return
		}
		after := r.URL.Query().Get("after")
		if after == version {
			select {
			case <-changed:
			case <-snapshots.stopping:
			case <-r.Context().Done():
				return
			case <-time.After(menuSnapshotLongestWait):
			}
			encoded, version, _ = read()
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Snapshot-Version", version)
		if after == version {
			// The reader's snapshot is current; it learns when that was confirmed.
			var latest menupb.MenuSnapshot
			if err := proto.Unmarshal(encoded, &latest); err == nil {
				w.Header().Set("X-Snapshot-Composed-At", latest.GeneratedAt.AsTime().UTC().Format(time.RFC3339Nano))
			}
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/x-protobuf")
		_, _ = w.Write(encoded)
	})
}

// writeMenuSnapshotURL publishes the URL atomically and readable only by
// this user, because it carries the client capability.
func writeMenuSnapshotURL(dataDirectory, snapshotURL string) error {
	temporary, err := os.CreateTemp(dataDirectory, ".menu-snapshot-url-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if _, err := temporary.WriteString(snapshotURL + "\n"); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), filepath.Join(dataDirectory, menuSnapshotURLFilename))
}

// readServingMenuSnapshot reports every installed account: its allowance,
// whether it can take new work for modelSlug and why not.
func readServingMenuSnapshot(dataDirectory string, router *routing.TaskRouter, modelSlug string) (*menupb.MenuSnapshot, error) {
	facts, unavailableReasons, err := router.ReadAccountAvailability(modelSlug)
	if err != nil {
		return nil, err
	}
	installed, err := accountapi.ReadPool(dataDirectory)
	if err != nil {
		return nil, err
	}
	if len(facts) != len(installed) {
		return nil, fmt.Errorf("account observations are not ready")
	}
	identities := map[store.AccountName]accountapi.PoolAccount{}
	for _, account := range installed {
		identities[store.AccountName(account.Name)] = account
	}
	snapshot := &menupb.MenuSnapshot{GeneratedAt: timestamppb.Now(), Connection: &menupb.Connection{}}
	for _, fact := range facts {
		identity := identities[fact.AccountName]
		sample := fact.AllowanceSample
		account := &menupb.Account{Key: string(fact.AccountName), AccountNumber: uint32(identity.ProviderNumber), Email: identity.Email, Plan: sample.Plan, State: menuAccountState(fact.AccountState), Usable: unavailableReasons[fact.AccountName] == "", UnavailableReason: unavailableReasons[fact.AccountName], OffersConfiguredModel: modelSlug == "" || slices.Contains(fact.SupportedModelSlugs, modelSlug)}
		if account.Usable {
			account.ReserveStatus = accountapi.ReserveStatus(fact, snapshot.GeneratedAt.AsTime())
		}
		account.Provider = menupb.Provider_PROVIDER_CHATGPT
		if identity.Provider == accountapi.ProviderClaude {
			account.Provider = menupb.Provider_PROVIDER_CLAUDE
			account.OffersConfiguredModel = true
		}
		if !sample.ObservedAt.IsZero() {
			observation := &menupb.AllowanceObservation{ObservedAt: timestamppb.New(sample.ObservedAt), Allowed: sample.Allowed, LimitReached: sample.LimitReached, AvailableResetCredits: sample.AvailableResetCredits}
			for _, window := range sample.Windows {
				observation.Windows = append(observation.Windows, &menupb.AllowanceWindow{PeriodSeconds: window.DurationSeconds, UsedPercent: window.UsedPercent, ResetAt: timestamppb.New(window.ResetsAt), ModelId: window.ModelID, ModelName: window.ModelName})
			}
			account.Allowance = observation
		}
		snapshot.Accounts = append(snapshot.Accounts, account)
	}
	return snapshot, nil
}

func menuAccountState(state accountapi.AccountState) menupb.AccountState {
	switch state {
	case accountapi.AccountReady:
		return menupb.AccountState_ACCOUNT_STATE_READY
	case accountapi.AccountLimited:
		return menupb.AccountState_ACCOUNT_STATE_LIMITED
	case accountapi.AccountSignInRequired:
		return menupb.AccountState_ACCOUNT_STATE_SIGN_IN_REQUIRED
	case accountapi.AccountCredentialsUnavailable:
		return menupb.AccountState_ACCOUNT_STATE_CREDENTIALS_UNAVAILABLE
	case accountapi.AccountUsageCheckFailed:
		return menupb.AccountState_ACCOUNT_STATE_USAGE_CHECK_FAILED
	default:
		return menupb.AccountState_ACCOUNT_STATE_UNSPECIFIED
	}
}

// servedListenAddress is where clients reach this installation: the address
// its proxy last served on, or the default before it first serves.
func servedListenAddress(dataDirectory string) string {
	published, err := os.ReadFile(filepath.Join(dataDirectory, menuSnapshotURLFilename))
	if err != nil {
		return listenAddress
	}
	snapshotURL, err := url.Parse(strings.TrimSpace(string(published)))
	if err != nil || validateLocalListenAddress(snapshotURL.Host) != nil {
		return listenAddress
	}
	return snapshotURL.Host
}
