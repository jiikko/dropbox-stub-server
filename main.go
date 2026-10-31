// Command dropbox-stub-server serves a local directory as a Dropbox account over the subset of the
// Dropbox HTTP API that apps commonly use. See README.md.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jiikko/dropbox-stub-server/internal/stub"
)

func main() {
	root := flag.String("root", "", "directory served as the Dropbox root (required)")
	addr := flag.String("addr", "127.0.0.1:8765", "listen address (loopback only)")
	quiet := flag.Bool("quiet", false, "do not log requests")
	flag.Parse()

	if err := run(*root, *addr, *quiet); err != nil {
		fmt.Fprintln(os.Stderr, "dropbox-stub-server:", err)
		os.Exit(1)
	}
}

func run(root, addr string, quiet bool) error {
	if root == "" {
		return errors.New("-root is required")
	}
	// The stub has no authentication and can read, move and delete everything under root,
	// so it must never be reachable from another machine.
	if err := requireLoopback(addr); err != nil {
		return err
	}
	var logger *log.Logger
	if !quiet {
		logger = log.New(os.Stderr, "", log.LstdFlags)
	}
	srv, err := stub.New(stub.Options{Root: root, Logger: logger})
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{Handler: srv, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.Serve(ln) }()
	// The ready line goes to stdout so scripts can wait for it.
	fmt.Printf("dropbox-stub-server listening on http://%s (root %s)\n", ln.Addr(), srv.Root())

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutdownCtx)
	}
}

func requireLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("-addr %q: %w", addr, err)
	}
	if host == "localhost" {
		return nil
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("-addr %q: only loopback addresses are allowed", addr)
	}
	return nil
}
