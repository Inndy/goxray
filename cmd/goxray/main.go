// Command goxray is a fail-closed recording proxy for Claude Code.
//
// Usage:
//
//	goxray [flags]              run the proxy (default)
//	goxray ls   [--session id]  list recorded exchanges
//	goxray show <id>            show one exchange's parsed metadata
//	goxray raw  <id> [req|res]  dump the verbatim recorded bytes
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"go.inndy.tw/goxray/internal/config"
	"go.inndy.tw/goxray/internal/model"
	"go.inndy.tw/goxray/internal/proxy"
	"go.inndy.tw/goxray/internal/store"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("")

	// Subcommand dispatch: the first bare arg selects a query command; anything
	// else (or nothing) runs the proxy.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "ls", "show", "raw":
			if err := runCLI(os.Args[1], os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, "goxray:", err)
				os.Exit(1)
			}
			return
		case "-h", "--help", "help":
			printUsage()
			return
		}
	}
	if err := serve(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "goxray:", err)
		os.Exit(1)
	}
}

func serve(args []string) error {
	cfg, err := config.Load(args)
	if err != nil {
		return err
	}

	st, err := store.Open(cfg.BlobsDir(), cfg.ManifestsDir(), cfg.IndexPath(),
		model.ProviderAnthropic, runtime.GOMAXPROCS(0))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	srv := proxy.New(cfg, st.Recorder)
	httpSrv := &http.Server{Addr: srv.Addr(), Handler: srv.Handler()}

	log.Printf("goxray listening on http://%s", srv.Addr())
	log.Printf("upstream: %s", cfg.Upstream)
	log.Printf("home:     %s", cfg.Home)
	log.Printf("point Claude Code at it:  export %s", cfg.BaseURLLine())

	// Graceful shutdown: stop accepting, drain the index queue, mark clean.
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	select {
	case err := <-errCh:
		st.Close()
		return err
	case <-sig:
		log.Printf("shutting down…")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(ctx)
		if err := st.Close(); err != nil {
			log.Printf("goxray: store close: %v", err)
		}
		return nil
	}
}

func printUsage() {
	fmt.Print(`goxray — fail-closed recording proxy for Claude Code

  goxray [flags]              run the proxy (default)
  goxray ls   [--session id]  list recorded exchanges (newest first)
  goxray show <id>            show one exchange's parsed metadata
  goxray raw  <id> [req|res]  dump verbatim recorded bytes (default req)

flags:
  --port N            listen port (env PROXY_PORT, default 5599)
  --home DIR          data dir (env GOXRAY_HOME, default ~/.goxray)
  --max-body-mb N     max request body MiB (default 50)
  --upstream URL      override upstream (env GOXRAY_UPSTREAM)
`)
}
