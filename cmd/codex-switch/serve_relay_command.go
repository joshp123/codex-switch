package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	accountapi "github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/clientcapability"
	"github.com/joshp123/codex-switch/internal/codexstate"
	"github.com/joshp123/codex-switch/internal/relay"
	"github.com/joshp123/codex-switch/internal/routing"
	"github.com/joshp123/codex-switch/internal/store"
	"github.com/joshp123/codex-switch/internal/usage"
)

const defaultUpstreamOrigin = "https://chatgpt.com"

// deployStatusEndpoint lets the installer distinguish generations of the same
// revision by pid, per the deployment contract.
const deployStatusEndpoint = "/codex-switch/deploy-status"

// Explicit stamps support build tools that cannot discover Git worktrees.
var sourceRevision string
var sourceModified string
var buildNumber string

// The installer writes this file for the length of a switch.
const switchStartedFilename = "switch-started"

// A failed account check says nothing about the build, so the installer tries
// the switch again instead of skipping the release.
const providerCheckFailedExitCode = 75

func buildNumberValue() int {
	n, _ := strconv.Atoi(buildNumber)
	return n
}

type deploymentActivity struct {
	failures      atomic.Int64
	lastCompleted atomic.Int64
}

// serveRelay runs the recovered serving path: SQLite-backed pins, the one
// routing rule, and the byte-preserving relay. The command layer owns the
// listener and signals; the relay owns admitted work during a drain.
func serveRelay(tcpListenAddress string) {
	// Every generation appends to one log in the data directory, so a blue-green
	// update keeps one timeline and the status command can name the file.
	proxyLog, err := os.OpenFile(proxyLogPath(dataDir()), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		log.Fatalf("open proxy log: %v", err)
	}
	defer proxyLog.Close()
	log.SetOutput(proxyLog)
	log.SetFlags(log.LstdFlags)
	capability, err := clientcapability.Create(dataDir())
	if err != nil {
		log.Fatal(err)
	}

	upstreamOriginValue := strings.TrimSpace(os.Getenv("CODEX_SWITCH_UPSTREAM_ORIGIN"))
	if upstreamOriginValue == "" {
		upstreamOriginValue = defaultUpstreamOrigin
	}
	upstreamOrigin, err := url.Parse(upstreamOriginValue)
	if err != nil {
		log.Fatal(fmt.Errorf("upstream origin: %w", err))
	}

	storeDatabase, err := store.OpenStoreDatabase(filepath.Join(dataDir(), "codex-switch.db"))
	if err != nil {
		log.Fatal(err)
	}
	defer storeDatabase.CloseStoreDatabase()
	if err := storeDatabase.StartModelRequestObservation(time.Now().UTC()); err != nil {
		log.Fatal(err)
	}

	upstreamTransport := relay.NewUpstreamTransport()
	accountMonitor := accountapi.NewMonitor(dataDir(), upstreamOrigin, codexstate.FindCodexExecutable(), &http.Client{Timeout: 30 * time.Second, Transport: upstreamTransport}, storeDatabase)
	claudeUpstreamOrigin, err := url.Parse("https://api.anthropic.com")
	if err != nil {
		log.Fatal(err)
	}
	accountMonitor.SetClaudeUpstreamOrigin(claudeUpstreamOrigin)
	// A process that starts during a switch is its candidate. Any other start
	// must succeed during a provider outage, so launchd can restart the proxy.
	if _, err := os.Stat(filepath.Join(dataDir(), switchStartedFilename)); err == nil {
		if err := accountMonitor.CheckProviderAccess(context.Background()); err != nil {
			failure := fmt.Sprintf("provider check failed: %s; old generation keeps serving", strings.ReplaceAll(err.Error(), "\n", "; "))
			log.Printf("process %d: %s", os.Getpid(), failure)
			fmt.Fprintln(os.Stderr, failure)
			os.Exit(providerCheckFailedExitCode)
		}
		log.Printf("process %d: provider check passed", os.Getpid())
	}
	usageLedger, err := usage.OpenUsageLedger(storeDatabase, dataDir(), &http.Client{Timeout: 15 * time.Second, Transport: upstreamTransport})
	if err != nil {
		log.Fatal(err)
	}
	factsContext, stopFacts := context.WithCancel(context.Background())
	var background sync.WaitGroup
	for _, run := range []func(context.Context){accountMonitor.Run, usageLedger.Run} {
		background.Add(1)
		go func() { defer background.Done(); run(factsContext) }()
	}
	defer func() { stopFacts(); background.Wait() }()
	taskRouter := routing.CreateTaskRouter(storeDatabase, accountMonitor)
	codexReader, err := codexstate.DefaultReader()
	if err != nil {
		log.Fatal(err)
	}
	snapshots := newMenuSnapshots(dataDir(), taskRouter, storeDatabase, usageLedger, codexReader)
	activity := &deploymentActivity{}
	relayHandler, err := relay.CreateTrafficRelayHandler(relay.RelayHandlerOptions{
		UpstreamOrigin:       upstreamOrigin,
		ClaudeUpstreamOrigin: claudeUpstreamOrigin,
		HTTPTransport:        upstreamTransport,
		AccountObserver:      accountMonitor,
		CacheAffinity:        storeDatabase,
		ClientCapability:     capability,
		TaskRouter:           taskRouter,
		CompletionLedger:     composeAfterCompletedResponse{ledger: storeDatabase, snapshots: snapshots, activity: activity},
		RequestCounters:      storeDatabase,
		OnFailure:            func() { activity.failures.Add(1) },
	})
	if err != nil {
		log.Fatal(err)
	}

	forwardProxy, err := relay.CreateClaudeForwardProxy(dataDir(), capability, relayHandler, snapshots.serveOrRelay(capability, profileOrRelay(capability, deployStatusOrRelay(capability, relayHandler, activity))))
	if err != nil {
		log.Fatal(err)
	}

	// A generation listens only once it can route: during an update, new
	// connections reach it as soon as it listens.
	accountMonitor.WaitForFirstRead()
	listener, err := openRelayListener(tcpListenAddress)
	if err != nil {
		log.Fatal(err)
	}
	revision, dirty := buildRevision()
	revision = revision[:min(12, len(revision))]
	if dirty {
		revision += " (modified)"
	}
	serving := fmt.Sprintf("codex-switch %s serving on %s, process %d", revision, "http://"+listener.Addr().String(), os.Getpid())
	log.Print(serving)
	fmt.Printf("codex-switch listening on %s\ncodex-switch log: %s\n", "http://"+listener.Addr().String(), proxyLogPath(dataDir()))

	clientURL := "http://" + listener.Addr().String() + clientcapability.PathPrefix(capability)
	snapshots.codexBaseURL = clientURL + "/backend-api/codex"
	background.Add(1)
	go func() { defer background.Done(); snapshots.Run(factsContext) }()
	if err := writeMenuSnapshotURL(dataDir(), clientURL+menuSnapshotEndpoint); err != nil {
		log.Fatalf("publish the menu snapshot URL: %v", err)
	}

	server := &http.Server{Handler: forwardProxy}
	serveFailed := make(chan error, 1)
	go func() { serveFailed <- server.Serve(listener) }()

	stopping := make(chan os.Signal, 1)
	signal.Notify(stopping, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stopping)

	select {
	case <-stopping:
		// The installer enforces 30 minutes from the start of the switch.
		// After the five-minute proof, admitted work has at most 25 minutes.
		drainStarted := time.Now()
		drainContext, cancelDrain := context.WithTimeout(context.Background(), 25*time.Minute)
		defer cancelDrain()
		log.Printf("process %d stopping: it accepts no new requests and finishes model work in progress", os.Getpid())
		snapshots.StopWaiting()
		relayHandler.CloseWebSocketsOnceIdle()
		forwardProxy.DrainConnections()
		if err := server.Shutdown(drainContext); err != nil {
			log.Printf("process %d: closing the listener failed: %v", os.Getpid(), err)
		}
		relayHandler.BeginRelayDrain()
		if err := relayHandler.WaitForRelayDrain(drainContext); err != nil {
			log.Printf("process %d: waiting for admitted connections failed: %v", os.Getpid(), err)
		}
		if drainContext.Err() != nil {
			log.Printf("process %d stopped after %s: the update deadline closed remaining connections", os.Getpid(), time.Since(drainStarted).Round(time.Second))
		} else {
			log.Printf("process %d stopped after %s: admitted work finished and connections closed", os.Getpid(), time.Since(drainStarted).Round(time.Second))
		}
	case err := <-serveFailed:
		if err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}
}

// deployStatusResponse identifies the running generation. Revision and pid
// are deploy-time evidence, deliberately absent from the menu contract.
type deployStatusResponse struct {
	Revision      string     `json:"revision"`
	Dirty         bool       `json:"dirty"`
	PID           int        `json:"pid"`
	Build         int        `json:"build"`
	Failures      int64      `json:"failures"`
	LastCompleted *time.Time `json:"last_completed"`
}

func deployStatusOrRelay(capability string, relayHandler http.Handler, activity *deploymentActivity) http.Handler {
	statusPath := clientcapability.PathPrefix(capability) + deployStatusEndpoint
	predecessorStatusPath := clientcapability.PathPrefix(capability) + "/status"
	revision, dirty := buildRevision()
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodGet && request.URL.Path == statusPath {
			writer.Header().Set("Content-Type", "application/json")
			var lastCompleted *time.Time
			if nanoseconds := activity.lastCompleted.Load(); nanoseconds != 0 {
				completed := time.Unix(0, nanoseconds).UTC()
				lastCompleted = &completed
			}
			_ = json.NewEncoder(writer).Encode(deployStatusResponse{Revision: revision, Dirty: dirty, PID: os.Getpid(), Build: buildNumberValue(), Failures: activity.failures.Load(), LastCompleted: lastCompleted})
			return
		}
		// Reject the retired control route without forwarding it to OpenAI.
		if request.URL.Path == predecessorStatusPath {
			http.Error(writer, "this codex-switch generation does not serve the predecessor status snapshot", http.StatusNotImplemented)
			return
		}
		relayHandler.ServeHTTP(writer, request)
	})
}

func buildRevision() (string, bool) {
	if sourceRevision != "" {
		return sourceRevision, sourceModified == "true"
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown", false
	}
	revision, dirty := "unknown", false
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			revision = setting.Value
		case "vcs.modified":
			dirty = setting.Value == "true"
		}
	}
	return revision, dirty
}

// printBinaryRevision lets release checks learn a binary's revision without
// starting it.
func printBinaryRevision(args []string) {
	if len(args) != 0 {
		log.Fatal("usage: codex-switch print-revision")
	}
	revision, _ := buildRevision()
	fmt.Println(revision)
}

// proxyLogPath is the serving proxy's one log file.
func proxyLogPath(dataDirectory string) string {
	return filepath.Join(dataDirectory, "proxy.log")
}
