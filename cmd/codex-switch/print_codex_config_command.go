package main

import (
	"flag"
	"fmt"
	"log"
	"os"

	"github.com/joshp123/codex-switch/internal/clientcapability"
)

func printCodexConfigCommand(args []string) {
	flags := flag.NewFlagSet("print-codex-config", flag.ExitOnError)
	address := flags.String("listen", servedListenAddress(dataDir()), "Codex Switch loopback address")
	_ = flags.Parse(args)
	if flags.NArg() != 0 {
		log.Fatal("usage: codex-switch print-codex-config [--listen 127.0.0.1:PORT]")
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
	fmt.Printf("openai_base_url = %q\n", "http://"+*address+clientcapability.PathPrefix(capability)+"/backend-api/codex")
}
