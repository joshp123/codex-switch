package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/joshp123/codex-switch/internal/accounts"
)

func resetCommand(args []string) {
	if len(args) != 1 || (args[0] != "codex" && args[0] != "claude") {
		log.Fatal("usage: codex-switch reset <codex|claude>")
	}
	if args[0] == "claude" {
		fmt.Fprintln(stdout, "Codex Switch spends no Claude reset. Spend one in Claude's Settings > Usage; `codex-switch status` lists those each account holds.")
		return
	}
	originValue := strings.TrimSpace(os.Getenv("CODEX_SWITCH_UPSTREAM_ORIGIN"))
	if originValue == "" {
		originValue = defaultUpstreamOrigin
	}
	origin, err := url.Parse(originValue)
	if err != nil {
		log.Fatal(err)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	result, err := accounts.ResetCodexPool(context.Background(), dataDir(), origin, client)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Fprintln(stdout, result.Message)
}

// autoResetCommand turns this installation's automatic resets on or off; see
// accounts.AutomaticResetsOn. Keep them on for one Mac only, one that is
// always on. No Claude reset is ever spent, so there is no switch for them.
func autoResetCommand(args []string) {
	// The switch names its provider: each provider's resets give back something
	// different, so each is turned on by itself. Only Codex resets are spent.
	if len(args) != 2 || args[0] != "codex" || (args[1] != "on" && args[1] != "off") {
		log.Fatal("usage: codex-switch auto-reset codex <on|off>")
	}
	if err := accounts.SetAutomaticResets(dataDir(), args[1] == "on"); err != nil {
		log.Fatal(err)
	}
	if args[1] == "on" {
		fmt.Fprintln(stdout, "Automatic Codex resets on: this Mac's proxy spends a Codex reset credit when the pool is out, or before a credit expires. Keep them on for one Mac only.")
		return
	}
	fmt.Fprintln(stdout, "Automatic Codex resets off: this Mac's proxy spends no reset credit by itself. `codex-switch reset codex` still spends one.")
}
