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
		fmt.Fprintln(stdout, "Claude resets are not supported yet.")
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
// always on.
func autoResetCommand(args []string) {
	if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
		log.Fatal("usage: codex-switch auto-reset <on|off>")
	}
	if err := accounts.SetAutomaticResets(dataDir(), args[0] == "on"); err != nil {
		log.Fatal(err)
	}
	if args[0] == "on" {
		fmt.Fprintln(stdout, "Automatic resets on: this Mac's proxy spends a Codex reset credit when the pool is out, or before a credit expires. Keep them on for one Mac only.")
		return
	}
	fmt.Fprintln(stdout, "Automatic resets off: this Mac's proxy spends no reset credit by itself. `codex-switch reset codex` still spends one.")
}
