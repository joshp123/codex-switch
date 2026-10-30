package main

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

const listenAddress = "127.0.0.1:2455"

func main() {
	log.SetFlags(0)
	// Codex Switch is the proxy: it reaches providers directly even when the
	// shell that started it routes through it with HTTPS_PROXY.
	http.DefaultTransport.(*http.Transport).Proxy = nil
	command, args := selectCommand(os.Args[1:])
	switch command {
	case "login":
		loginCommand(args)
	case "accounts":
		accountsCommand(args)
	case "remove":
		removeCommand(args)
	case "reserve":
		reserveCommand(args)
	case "serve":
		serveCommand(args)
	case "print-codex-config":
		printCodexConfigCommand(args)
	case "print-claude-config":
		printClaudeConfigCommand(args)
	case "print-build":
		fmt.Println(buildNumberValue())
	case "print-revision":
		printBinaryRevision(args)
	default:
		status(args)
	}
}

func selectCommand(args []string) (string, []string) {
	if len(args) == 0 {
		return "status", args
	}
	switch args[0] {
	case "login", "accounts", "remove", "reserve", "serve", "status", "print-codex-config", "print-claude-config", "print-revision", "print-build":
		return args[0], args[1:]
	default:
		return "status", args
	}
}

func dataDir() string {
	if value := strings.TrimSpace(os.Getenv("CODEX_SWITCH_DATA_DIR")); value != "" {
		return value
	}
	home, err := os.UserHomeDir()
	if err != nil {
		log.Fatal(err)
	}
	return filepath.Join(home, "Library", "Application Support", "codex-switch")
}

func validateLocalListenAddress(address string) error {
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return fmt.Errorf("invalid local listen address: %w", err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return errors.New("codex-switch may listen only on a numeric loopback address")
	}
	return nil
}
