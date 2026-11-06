package main

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	accountapi "github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/clientcapability"
	"github.com/joshp123/codex-switch/internal/routing"
)

// moveTasksEndpoint runs in the serving proxy, which holds the current
// allowance readings that decide where new work goes.
const moveTasksEndpoint = "/codex-switch/move-tasks"

const moveTasksUsage = `usage: codex-switch move-tasks claude|codex
  Moves every Claude or Codex task to the account new work goes to now.`

func moveTasksCommand(args []string) {
	if len(args) != 1 {
		log.Fatal(moveTasksUsage)
	}
	_, providerName, known := providerForMoveTasks(args[0])
	if !known {
		log.Fatal(moveTasksUsage)
	}
	endpointURL, err := servingProxyEndpointURL(dataDir(), moveTasksEndpoint)
	if err != nil {
		log.Fatalf("cannot reach the proxy: %v", err)
	}
	client := http.Client{Timeout: 10 * time.Second}
	response, err := client.Post(endpointURL+"?provider="+url.QueryEscape(args[0]), "text/plain", nil)
	if err != nil {
		log.Fatalf("the proxy did not answer: %v", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 4096))
	if err != nil {
		log.Fatalf("read the proxy's answer: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		log.Fatalf("%s: %s", providerName, strings.TrimSpace(string(body)))
	}
	fmt.Printf("%s: %s. Each moved task re-reads its conversation on that account at its next request.\n", providerName, strings.TrimSpace(string(body)))
}

func moveTasksOrRelay(capability string, taskRouter *routing.TaskRouter, next http.Handler) http.Handler {
	movePath := clientcapability.PathPrefix(capability) + moveTasksEndpoint
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != movePath {
			next.ServeHTTP(writer, request)
			return
		}
		if request.Method != http.MethodPost {
			http.Error(writer, "use POST", http.StatusMethodNotAllowed)
			return
		}
		provider, providerName, known := providerForMoveTasks(request.URL.Query().Get("provider"))
		if !known {
			http.Error(writer, "provider must be claude or codex", http.StatusBadRequest)
			return
		}
		outcome, err := taskRouter.MoveTasksToNewWorkAccount(provider)
		if err != nil {
			log.Printf("move-tasks command: %s tasks did not move: %v", providerName, err)
			http.Error(writer, err.Error(), http.StatusConflict)
			return
		}
		log.Printf("move-tasks command: %s %s", providerName, outcome)
		writer.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(writer, outcome)
	})
}

func providerForMoveTasks(name string) (accountapi.Provider, string, bool) {
	switch name {
	case "claude":
		return accountapi.ProviderClaude, "Claude", true
	case "codex":
		return accountapi.ProviderCodex, "Codex", true
	}
	return "", "", false
}

// servingProxyEndpointURL is a private endpoint of the proxy that last
// published its menu snapshot URL.
func servingProxyEndpointURL(dataDirectory, endpoint string) (string, error) {
	published, err := os.ReadFile(filepath.Join(dataDirectory, menuSnapshotURLFilename))
	if err != nil {
		return "", fmt.Errorf("no proxy has served from %s: %w", dataDirectory, err)
	}
	endpointURL, err := url.Parse(strings.TrimSpace(string(published)))
	if err != nil || endpointURL.Scheme != "http" {
		return "", errors.New(menuSnapshotURLFilename + " does not hold a local proxy URL")
	}
	if err := validateLocalListenAddress(endpointURL.Host); err != nil {
		return "", err
	}
	endpointURL.Path = strings.TrimSuffix(endpointURL.Path, menuSnapshotEndpoint) + endpoint
	endpointURL.RawQuery = ""
	return endpointURL.String(), nil
}
