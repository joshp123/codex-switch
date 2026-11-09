package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/joshp123/codex-switch/internal/clientcapability"
	"github.com/joshp123/codex-switch/internal/relay"
)

// printClaudeConfigCommand prints settings to merge into the chosen client's
// existing file. It never writes a Claude Code or Pi configuration.
func printClaudeConfigCommand(args []string) {
	flags := flag.NewFlagSet("print-claude-config", flag.ExitOnError)
	address := flags.String("listen", servedListenAddress(dataDir()), "Codex Switch loopback address")
	pi := flags.Bool("pi", false, "print Pi's anthropic provider entry")
	_ = flags.Parse(args)
	if flags.NArg() != 0 {
		log.Fatal("usage: codex-switch print-claude-config [--pi] [--listen 127.0.0.1:PORT]")
	}
	if err := validateLocalListenAddress(*address); err != nil {
		log.Fatal(err)
	}
	dataDirectory := dataDir()
	if err := os.MkdirAll(dataDirectory, 0o700); err != nil {
		log.Fatal(err)
	}
	capability, err := clientcapability.Create(dataDirectory)
	if err != nil {
		log.Fatal(err)
	}
	baseURL := "http://" + *address + clientcapability.PathPrefix(capability) + "/anthropic"
	var configuration any = struct {
		Env map[string]string `json:"env"`
	}{Env: map[string]string{
		"HTTPS_PROXY":         relay.ClaudeProxyURL(*address, capability),
		"NODE_EXTRA_CA_CERTS": relay.ClaudeProxyCAPath(dataDirectory),
	}}
	if *pi {
		type provider struct {
			BaseURL string `json:"baseUrl"`
			APIKey  string `json:"apiKey"`
		}
		configuration = struct {
			Providers map[string]provider `json:"providers"`
		}{Providers: map[string]provider{"anthropic": {BaseURL: baseURL, APIKey: "sk-ant-oat01-codex-switch"}}}
	} else if _, err := relay.CreateClaudeProxyCertificate(dataDirectory); err != nil {
		log.Fatal(err)
	}
	encoded, err := json.MarshalIndent(configuration, "", "  ")
	if err != nil {
		log.Fatal(err)
	}
	fmt.Println(string(encoded))
}
