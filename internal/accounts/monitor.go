package accounts

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/joshp123/codex-switch/internal/store"
)

// Healthy accounts are read once a minute; waiting work asks for a read at
// most every five seconds. Model lists change only when a provider ships or
// withdraws a model, so they are read every 15 minutes, and again whenever
// waiting work asks, which covers a model no account was known to offer.
// Network reads never run on the routing path.
const (
	accountReadInterval          = time.Minute
	requestedAccountReadInterval = 5 * time.Second
	modelListReadInterval        = 15 * time.Minute
	accountReadTimeout           = 15 * time.Second
)

// AccountRoutingFact keeps provider eligibility and its matching credentials in one selection unit.
type AccountRoutingFact struct {
	Provider           Provider
	ClaudeRestrictions map[string]ClaudeRestriction
	AccountName        store.AccountName
	DisplayName        string
	Reserve            AccountReserve
	Credentials        Credentials
	AccountState       AccountState
	AccountStateCause  error
	AllowanceSample    AllowanceSample
	RefreshedAt        time.Time
	// Nil means the catalogue has not been observed; an empty slice means no models.
	SupportedModelSlugs []string
	// ModelListCause is why the latest model-list read failed; nil after a successful read.
	ModelListCause error
}

// Monitor watches every account in the pool: its credentials, provider-reported
// allowances and model availability. Routing reads the
// latest facts without waiting on the network.
type Monitor struct {
	claudeOrigin    *url.URL
	dataDirectory   string
	usageURL        string
	modelsURL       string
	codexExecutable string
	httpClient      *http.Client
	storeDatabase   *store.StoreDatabase

	// readInterval is accountReadInterval; the system test shortens it.
	readInterval time.Duration

	mutex         sync.Mutex
	accounts      map[store.AccountName]*monitoredAccount
	readRequested chan struct{}
	firstReadDone chan struct{}
}

// monitoredAccount has at most one read in flight. The request path may change
// its fact during that read; see keepRequestPathObservations.
type monitoredAccount struct {
	fact                     AccountRoutingFact
	savedLogin               *savedLogin
	lastReadStarted          time.Time
	lastModelListReadStarted time.Time
	reading                  bool
}

// NewMonitor reads accounts from upstreamOrigin, asking for the model catalogue
// that codexExecutable's version of Codex would see.
func NewMonitor(dataDirectory string, upstreamOrigin *url.URL, codexExecutable string, httpClient *http.Client, storeDatabase *store.StoreDatabase) *Monitor {
	return &Monitor{
		claudeOrigin:    &url.URL{Scheme: "https", Host: "api.anthropic.com"},
		dataDirectory:   dataDirectory,
		usageURL:        upstreamOrigin.ResolveReference(&url.URL{Path: "/backend-api/wham/usage"}).String(),
		modelsURL:       upstreamOrigin.ResolveReference(&url.URL{Path: "/backend-api/codex/models"}).String(),
		codexExecutable: codexExecutable,
		httpClient:      httpClient,
		storeDatabase:   storeDatabase,
		readInterval:    accountReadInterval,
		accounts:        map[store.AccountName]*monitoredAccount{},
		readRequested:   make(chan struct{}, 1),
		firstReadDone:   make(chan struct{}),
	}
}

// SetAccountReadInterval changes how often each account is read, before Run starts.
func (monitor *Monitor) SetAccountReadInterval(interval time.Duration) {
	monitor.readInterval = interval
}

// WaitForFirstRead returns once every account has been read once,
// successfully or not.
func (monitor *Monitor) WaitForFirstRead() {
	<-monitor.firstReadDone
}

// Run discovers accounts and reads them until ctx ends. Each account is read
// independently, so a slow account never delays another, and discovery goes
// on while the first reads finish.
func (monitor *Monitor) Run(ctx context.Context) {
	var firstReads, reads sync.WaitGroup
	defer func() { firstReads.Wait(); reads.Wait() }()
	nextRead := monitor.startDueReads(ctx, &firstReads, false)
	go func() { firstReads.Wait(); close(monitor.firstReadDone) }()
	timer := time.NewTimer(time.Until(nextRead))
	defer timer.Stop()
	for {
		readRequested := false
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		case <-monitor.readRequested:
			readRequested = true
		}
		nextRead = monitor.startDueReads(ctx, &reads, readRequested)
		timer.Reset(time.Until(nextRead))
	}
}

func (monitor *Monitor) startDueReads(ctx context.Context, reads *sync.WaitGroup, readRequested bool) time.Time {
	now := time.Now()
	nextRead := time.Time{}
	pool, err := ReadPool(monitor.dataDirectory)
	if err != nil {
		log.Printf("could not read the account pool: %v", err)
		return now.Add(monitor.readInterval)
	}
	interval := monitor.readInterval
	if readRequested {
		interval = requestedAccountReadInterval
	}
	monitor.mutex.Lock()
	defer monitor.mutex.Unlock()
	for _, account := range pool {
		monitored := monitor.monitoredAccountLocked(store.AccountName(account.Name), account.Provider)
		if monitored.reading || now.Sub(monitored.lastReadStarted) < interval {
			dueAt := monitored.lastReadStarted.Add(monitor.readInterval)
			if monitored.reading && !dueAt.After(now) {
				dueAt = now.Add(monitor.readInterval)
			}
			if nextRead.IsZero() || dueAt.Before(nextRead) {
				nextRead = dueAt
			}
			continue
		}
		monitored.reading = true
		monitored.lastReadStarted = now
		dueAt := now.Add(monitor.readInterval)
		if nextRead.IsZero() || dueAt.Before(nextRead) {
			nextRead = dueAt
		}
		readModelList := readRequested || monitored.fact.SupportedModelSlugs == nil || monitored.fact.ModelListCause != nil ||
			time.Since(monitored.lastModelListReadStarted) >= modelListReadInterval
		if readModelList {
			monitored.lastModelListReadStarted = monitored.lastReadStarted
		}
		reads.Add(1)
		go func(scheduled AccountRoutingFact) {
			defer reads.Done()
			monitor.readAccount(ctx, account, monitored, scheduled, readModelList)
		}(monitored.fact)
	}
	if nextRead.IsZero() {
		nextRead = now.Add(monitor.readInterval)
	}
	return nextRead
}

func (monitor *Monitor) readAccount(ctx context.Context, account PoolAccount, monitored *monitoredAccount, scheduled AccountRoutingFact, readModelList bool) {
	var read AccountRoutingFact
	if account.Provider == ProviderClaude {
		read = monitor.observeClaudeAccount(ctx, account, monitored.savedLogin, scheduled)
	} else {
		read = monitor.observeAccount(ctx, account, monitored.savedLogin, scheduled, readModelList)
	}
	read.Reserve = account.Reserve
	monitor.mutex.Lock()
	latest := monitored.fact
	read = keepRequestPathObservations(read, scheduled, latest)
	read.DisplayName = account.DisplayName
	availabilityChange := describeAccountAvailabilityChange(latest, read, account.Email)
	monitored.fact = read
	monitored.reading = false
	monitor.mutex.Unlock()
	if availabilityChange != "" && ctx.Err() == nil {
		log.Print(availabilityChange)
	}
}

// keepRequestPathObservations keeps what the request path learnt while a read
// was in flight: credentials it refreshed after a 401, a model OpenAI
// rejected, and a sign-in OpenAI demanded. They are newer than the read.
func keepRequestPathObservations(read, scheduled, latest AccountRoutingFact) AccountRoutingFact {
	if latest.Reserve != scheduled.Reserve {
		read.Reserve = latest.Reserve
	}
	if read.Provider == ProviderClaude {
		read.AllowanceSample = mergeClaudeAllowance(read.AllowanceSample, latest.AllowanceSample)
		read.ClaudeRestrictions = mergeClaudeRestrictions(read.ClaudeRestrictions, latest.ClaudeRestrictions)
		updateClaudeSharedState(&read, time.Now())
	}
	if latest.Credentials != scheduled.Credentials {
		read.Credentials = latest.Credentials
	}
	if !slices.Equal(latest.SupportedModelSlugs, scheduled.SupportedModelSlugs) {
		read.SupportedModelSlugs = latest.SupportedModelSlugs
	}
	if latest.AccountState == AccountSignInRequired && scheduled.AccountState != AccountSignInRequired && read.Credentials == latest.Credentials {
		read.AccountState = latest.AccountState
		read.AccountStateCause = latest.AccountStateCause
	}
	return read
}

// ReadAccountRoutingFacts joins the latest facts to current pool membership.
// Removed or replaced accounts cannot admit new work before their next read.
func (monitor *Monitor) ReadAccountRoutingFacts() ([]AccountRoutingFact, error) {
	pool, err := ReadPool(monitor.dataDirectory)
	if err != nil {
		return nil, err
	}
	monitor.mutex.Lock()
	defer monitor.mutex.Unlock()
	facts := make([]AccountRoutingFact, 0, len(pool))
	for _, account := range pool {
		monitored, known := monitor.accounts[store.AccountName(account.Name)]
		if known && monitored.fact.Credentials.AccountID == account.AccountID {
			fact := monitored.fact
			fact.DisplayName = account.DisplayName
			fact.Reserve = account.Reserve
			if fact.Reserve != monitored.fact.Reserve {
				if change := describeAccountAvailabilityChange(monitored.fact, fact, account.Email); change != "" {
					log.Print(change)
				}
				monitored.fact.Reserve = fact.Reserve
			}
			facts = append(facts, fact)
		}
	}
	return facts, nil
}

// RequestAccountRoutingFactsRefresh asks for every account to be read again
// soon, because work is waiting for capacity.
func (monitor *Monitor) RequestAccountRoutingFactsRefresh() {
	select {
	case monitor.readRequested <- struct{}{}:
	default:
	}
}

// ExcludeUnsupportedAccountModel records that OpenAI rejected modelSlug for the
// account before admitting work. An in-flight catalogue read cannot overwrite
// this newer observation.
func (monitor *Monitor) ExcludeUnsupportedAccountModel(accountName store.AccountName, modelSlug string) {
	monitor.mutex.Lock()
	monitored := monitor.monitoredAccountLocked(accountName)
	monitored.fact.SupportedModelSlugs = slices.DeleteFunc(slices.Clone(monitored.fact.SupportedModelSlugs), func(slug string) bool { return slug == modelSlug })
	monitor.mutex.Unlock()
	monitor.RequestAccountRoutingFactsRefresh()
}

// RefreshAccountCredentialsAfterUnauthorized prepares the account for the next routed request.
func (monitor *Monitor) RefreshAccountCredentialsAfterUnauthorized(accountName store.AccountName, rejected Credentials) error {
	pool, err := ReadPool(monitor.dataDirectory)
	if err != nil {
		return err
	}
	index := slices.IndexFunc(pool, func(account PoolAccount) bool { return store.AccountName(account.Name) == accountName })
	if index < 0 {
		return fmt.Errorf("account is no longer in the pool")
	}
	monitor.mutex.Lock()
	monitored := monitor.monitoredAccountLocked(accountName, pool[index].Provider)
	monitor.mutex.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), accountReadTimeout)
	refreshed, refreshErr := monitored.savedLogin.RefreshAfterUnauthorized(ctx, rejected)
	cancel()
	monitor.mutex.Lock()
	defer monitor.mutex.Unlock()
	if refreshErr != nil {
		if requiresSignIn(refreshErr) && monitored.fact.AccountName != "" && monitored.fact.Credentials == rejected {
			monitored.fact.AccountState = AccountSignInRequired
			monitored.fact.AccountStateCause = refreshErr
		}
		return refreshErr
	}
	if monitored.fact.AccountName != "" {
		monitored.fact.Credentials = refreshed
	}
	return nil
}

func (monitor *Monitor) monitoredAccountLocked(accountName store.AccountName, providers ...Provider) *monitoredAccount {
	monitored, known := monitor.accounts[accountName]
	if !known {
		monitored = &monitoredAccount{savedLogin: newSavedLogin(filepath.Join(monitor.dataDirectory, string(accountName)), monitor.httpClient)}
		if len(providers) > 0 {
			monitored.savedLogin.provider = providers[0]
		}
		monitor.accounts[accountName] = monitored
	}
	return monitored
}
