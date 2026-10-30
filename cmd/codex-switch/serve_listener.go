package main

import (
	"context"
	"flag"
	"log"
	"net"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func serveCommand(args []string) {
	flags := flag.NewFlagSet("serve", flag.ExitOnError)
	tcpListenAddress := flags.String("listen", listenAddress, "numeric loopback address and port")
	_ = flags.Parse(args)
	if flags.NArg() != 0 {
		log.Fatal("usage: codex-switch serve [--listen LOOPBACK:PORT]")
	}
	serveRelay(*tcpListenAddress)
}

// openRelayListener sets SO_REUSEPORT on every TCP generation, forever: the
// deployment hand-off depends on two generations binding the same port, and
// a generation without the option gets EADDRINUSE against one that has it.
func openRelayListener(tcpListenAddress string) (net.Listener, error) {
	tcpListenAddress = strings.TrimSpace(tcpListenAddress)
	if err := validateLocalListenAddress(tcpListenAddress); err != nil {
		return nil, err
	}
	config := net.ListenConfig{Control: func(network, address string, raw syscall.RawConn) error {
		var optErr error
		err := raw.Control(func(fd uintptr) {
			optErr = unix.SetsockoptInt(int(fd), unix.SOL_SOCKET, unix.SO_REUSEPORT, 1)
		})
		if err != nil {
			return err
		}
		return optErr
	}}
	return config.Listen(context.Background(), "tcp", tcpListenAddress)
}
