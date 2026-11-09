package main

import (
	"flag"
	"fmt"
	"log"
	"strconv"

	accountapi "github.com/joshp123/codex-switch/internal/accounts"
	"github.com/joshp123/codex-switch/internal/codexstate"
	"github.com/joshp123/codex-switch/internal/store"
)

func loginCommand(args []string) {
	flags := flag.NewFlagSet("login", flag.ExitOnError)
	codex := flags.String("codex", codexstate.FindCodexExecutable(), "official Codex executable")
	claude := flags.Bool("claude", false, "sign in to a Claude subscription account")
	_ = flags.Parse(args)
	if flags.NArg() > 1 {
		log.Fatal("usage: codex-switch login [--claude] [\"Codex N\" | \"Claude N\" | account key]")
	}
	target := flags.Arg(0)
	var replacing store.AccountKey
	if target == "" {
		fmt.Fprintln(stdout, "Sign in to the account you want to add")
	} else {
		pool, err := accountapi.ReadPool(dataDir())
		if err != nil {
			log.Fatal(err)
		}
		for _, account := range pool {
			if target == string(account.Key) || target == account.DisplayName {
				replacing = account.Key
				fmt.Fprintf(stdout, "Sign in again to %s\n", account.DisplayName)
				break
			}
		}
		if replacing == "" {
			log.Fatalf("unknown account %s", target)
		}
	}
	var published accountapi.PoolAccount
	var err error
	if *claude {
		published, err = accountapi.SignInClaude(dataDir(), replacing)
	} else {
		published, err = accountapi.SignIn(dataDir(), *codex, replacing)
	}
	if err != nil {
		log.Fatal(err)
	}
	pool, err := accountapi.ReadPool(dataDir())
	if err != nil {
		log.Fatal(err)
	}
	displayName, _ := accountapi.AccountDisplayName(pool, published.Key)
	if target != "" {
		fmt.Fprintf(stdout, "Updated %s  %s\n", displayName, published.Email)
		return
	}
	fmt.Fprintf(stdout, "Added %s  %s\n", displayName, published.Email)
}

func accountsCommand(args []string) {
	if len(args) != 0 {
		log.Fatal("usage: codex-switch accounts")
	}
	pool, err := accountapi.ReadPool(dataDir())
	if err != nil {
		log.Fatal(err)
	}
	if len(pool) == 0 {
		fmt.Fprintln(stdout, "No accounts. Run codex-switch login.")
		return
	}
	for _, account := range pool {
		fmt.Fprintf(stdout, "%s\t%s\n", account.DisplayName, account.Key)
	}
}

func removeCommand(args []string) {
	if len(args) != 1 {
		log.Fatal("usage: codex-switch remove \"Codex N\" | \"Claude N\" | account key")
	}
	removed, removedPins, err := accountapi.RemoveAccount(dataDir(), args[0])
	if err != nil {
		log.Fatal(err)
	}
	fmt.Fprintf(stdout, "Removed %s (%s) and %d thread pins.\n", removed.DisplayName, removed.Email, removedPins)
}

func reserveCommand(args []string) {
	const usage = "usage: codex-switch reserve <account> <soft-percent> <hard-percent>\n       codex-switch reserve <account> off"
	if len(args) != 3 && (len(args) != 2 || args[1] != "off") {
		log.Fatal(usage)
	}
	reserve := accountapi.AccountReserve{}
	if len(args) == 3 {
		soft, softErr := strconv.Atoi(args[1])
		hard, hardErr := strconv.Atoi(args[2])
		if softErr != nil || hardErr != nil {
			log.Fatal(usage)
		}
		reserve = accountapi.AccountReserve{SoftPercent: soft, HardPercent: hard}
	}
	account, err := accountapi.SetAccountReserve(dataDir(), args[0], reserve)
	if err != nil {
		log.Fatal(err)
	}
	if reserve == (accountapi.AccountReserve{}) {
		fmt.Fprintf(stdout, "%s: reserve off\n", account.DisplayName)
	} else {
		fmt.Fprintf(stdout, "%s: soft reserve %d%%; hard reserve %d%%\n", account.DisplayName, reserve.SoftPercent, reserve.HardPercent)
	}
}
