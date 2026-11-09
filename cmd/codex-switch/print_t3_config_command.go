package main

import (
	"flag"
	"fmt"
	"log"

	"github.com/joshp123/codex-switch/internal/clientcapability"
)

func printT3ConfigCommand(args []string) {
	flags := flag.NewFlagSet("print-t3-config", flag.ExitOnError)
	_ = flags.Parse(args)
	if flags.NArg() != 0 {
		log.Fatal("usage: codex-switch print-t3-config")
	}
	address := servedListenAddress(dataDir())
	if err := validateLocalListenAddress(address); err != nil {
		log.Fatal(err)
	}
	capability, err := clientcapability.Read(dataDir())
	if err != nil {
		log.Fatal(err)
	}
	fmt.Fprintln(stdout, "In T3 Code, open Settings → Providers → Usage providers → Add hub.")
	fmt.Fprintf(stdout, "Hub URL: http://%s\nManagement key: %s\n", address, capability)
}
