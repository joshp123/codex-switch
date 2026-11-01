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
		fmt.Println("Claude resets are not supported yet.")
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
	fmt.Println(result.Message)
}
