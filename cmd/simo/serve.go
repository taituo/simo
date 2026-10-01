package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/taituo/simo/internal/mcpserver"
)

// stdin is the MCP stdio input; tests replace it.
var stdin io.Reader = os.Stdin

func cmdServe(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(stderr)
	worlds := fs.String("worlds", "worlds", "directory of worlds (author tools create them here)")
	world := fs.String("world", "", "serve only this world: a name in --worlds, or a path to a world directory")
	roles := fs.String("role", "observer", "comma-separated roles: observer, author, admin")
	httpAddr := fs.String("http", "", "serve streamable HTTP on this address, e.g. 127.0.0.1:8765, instead of stdio")
	maxTicks := fs.Int64("max-device-ticks", 0, "cap on devices x ticks for preview_seed and create_world (default 2e9)")
	workers := fs.Int("workers", 0, "engine worker goroutines (0 = all CPUs)")
	pos, err := parseArgs(fs, args)
	if err != nil {
		return err
	}
	if len(pos) != 0 {
		fmt.Fprintln(stderr, "simo serve takes no positional arguments")
		fs.Usage()
		return errUsage
	}
	cfg := mcpserver.Config{WorldsDir: *worlds, World: *world, Version: version, MaxDeviceTicks: *maxTicks, Workers: *workers}
	if strings.ContainsAny(*world, `/\`) || isWorld(*world) {
		cfg.WorldsDir, cfg.World = filepath.Dir(filepath.Clean(*world)), filepath.Base(filepath.Clean(*world))
	}
	if cfg.World != "" && !isWorld(filepath.Join(cfg.WorldsDir, cfg.World)) {
		return fmt.Errorf("%s is not a world directory (build one with: simo run SEED --db DIR)", filepath.Join(cfg.WorldsDir, cfg.World))
	}
	for _, r := range strings.Split(*roles, ",") {
		if r = strings.TrimSpace(r); r != "" {
			cfg.Roles = append(cfg.Roles, r)
		}
	}
	srv, closeFn, err := mcpserver.New(cfg)
	if err != nil {
		return err
	}
	defer closeFn()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if *httpAddr == "" {
		return srv.ServeStdio(ctx, stdin, stdout)
	}
	if host, _, err := net.SplitHostPort(*httpAddr); err == nil {
		if ip := net.ParseIP(host); host == "" || (ip != nil && !ip.IsLoopback() && host != "localhost") {
			fmt.Fprintln(stderr, "warning: listening beyond loopback; anyone who can reach this address can use the server")
		}
	}
	mux := http.NewServeMux()
	mux.Handle("/mcp", srv.HTTPHandler())
	hs := &http.Server{Addr: *httpAddr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- hs.ListenAndServe() }()
	fmt.Fprintf(stderr, "simo MCP server on http://%s/mcp (roles: %s, worlds: %s)\n", *httpAddr, strings.Join(cfg.Roles, ", "), cfg.WorldsDir)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := hs.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
		return nil
	}
}
