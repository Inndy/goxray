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
	"sync"
	"syscall"
	"time"

	"go.inndy.tw/goxray/internal/config"
	"go.inndy.tw/goxray/internal/model"
	"go.inndy.tw/goxray/internal/proxy"
	"go.inndy.tw/goxray/internal/store"
	"go.inndy.tw/goxray/internal/web"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("")

	// Subcommand dispatch: the first bare arg selects a query command; anything
	// else (or nothing) runs the proxy.
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "ls", "show", "raw", "migrate-usage", "archive", "fsck":
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

	st, err := store.Open(cfg.BlobsDir(), cfg.ManifestsDir(), cfg.ArchivesDir(), cfg.IndexPath(),
		model.ProviderAnthropic, runtime.GOMAXPROCS(0))
	if err != nil {
		return fmt.Errorf("open store: %w", err)
	}

	// Fold idle sessions into per-session archives in the background. It polls
	// the index from its own goroutine and never touches the recording path, so
	// it cannot affect the fail-closed guarantee.
	sweeper := store.NewSweeper(cfg.BlobsDir(), cfg.ManifestsDir(), cfg.ArchivesDir(),
		st.Index, cfg.ArchiveAfter)
	// A rebuilt archive is installed by renaming over the old file, which Windows
	// refuses while this process still holds that file open — as the dashboard's
	// blob cache does for the last two archives anyone viewed.
	sweeper.ReleaseWith(st.ReleaseArchive)
	sweeper.Start()
	defer sweeper.Stop()

	srv := proxy.New(cfg, st.Recorder)
	srv.SetStreams(func(conn *model.Exchange) (proxy.StreamLog, error) {
		return st.Recorder.OpenStream(conn)
	})
	httpSrv := &http.Server{Addr: srv.Addr(), Handler: srv.Handler()}

	log.Printf("goxray listening on http://%s", srv.Addr())
	log.Printf("upstream: %s", cfg.Upstream)
	log.Printf("home:     %s", cfg.Home)
	if cfg.ArchiveEnabled() {
		log.Printf("archiving sessions idle for %s", cfg.ArchiveAfter)
	}
	log.Printf("point Claude Code at it:  export %s", cfg.BaseURLLine())
	log.Printf("point Codex at it:        %s  (in ~/.codex/config.toml)", cfg.CodexLine())

	// Graceful shutdown: stop accepting, drain the index queue, mark clean.
	errCh := make(chan error, 1)
	go func() { errCh <- httpSrv.ListenAndServe() }()

	// The dashboard is a separate server on a separate port with read-only
	// access to the store. It observes the recorder to stream live turns; the
	// observer is non-blocking, so it cannot stall the fail-closed path.
	var uiSrv *http.Server
	if cfg.UIEnabled() {
		hub := web.NewHub()
		st.Recorder.SetObserver(hub)
		addr := fmt.Sprintf("127.0.0.1:%d", cfg.UIPort)
		uiSrv = &http.Server{
			Addr:    addr,
			Handler: web.Handler(web.FromStore(st), hub, cfg.UIPort),
		}
		log.Printf("dashboard: http://localhost:%d", cfg.UIPort)
		go func() {
			if err := uiSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				// A dead dashboard must not take the proxy down with it.
				log.Printf("goxray: dashboard: %v", err)
			}
		}()
	}

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	// Proxy first, with the whole deadline: its handlers are mid-commit and
	// deserve every second. The dashboard is hard-closed — its SSE streams never
	// go idle, so a graceful Shutdown would just burn the deadline waiting on
	// them (and previously starved the proxy of it).
	//
	// WebSockets drain alongside HTTP, not after it: Shutdown does not wait for
	// hijacked connections, and one long SSE turn must not spend their share of
	// the deadline before they are even asked to close.
	shutdown := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		dl, _ := ctx.Deadline()
		var wg sync.WaitGroup
		wg.Go(func() { srv.CloseStreams(dl) })
		if err := httpSrv.Shutdown(ctx); err != nil {
			log.Printf("goxray: proxy shutdown: %v (a stream may still be mid-turn)", err)
		}
		wg.Wait()
		if uiSrv != nil {
			_ = uiSrv.Close()
		}
	}

	select {
	case err := <-errCh:
		// The listener died, but sockets it already handed over are still
		// recording into the store about to close.
		srv.CloseStreams(time.Now().Add(5 * time.Second))
		if uiSrv != nil {
			_ = uiSrv.Close()
		}
		sweeper.Stop()
		st.Close()
		return err
	case <-sig:
		log.Printf("shutting down…")
		shutdown()
		// Before the store: a pass in flight is holding the index open.
		sweeper.Stop()
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
  goxray migrate-usage        backfill usage + ctx window from blobs (--apply)
  goxray archive              fold idle sessions into archives (--apply)
  goxray fsck                 verify archive integrity

flags:
  --port N            listen port (env PROXY_PORT, default 5599)
  --ui-port N         dashboard port (env GOXRAY_UI_PORT, default 0 = off)
  --home DIR          data dir (env GOXRAY_HOME, default ~/.goxray)
  --max-body-mb N     max request body MiB (default 50)
  --upstream URL      override upstream (env GOXRAY_UPSTREAM)
  --archive-after D   archive idle sessions after D (env GOXRAY_ARCHIVE_AFTER,
                      default 30m, 0 = off)
`)
}
