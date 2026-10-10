// Command lampa-web-builder builds the Lampa web frontend from upstream with
// our patches and serves it over a minimal HTTP API.
package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/huhen/lampa-web-builder/internal/api"
	"github.com/huhen/lampa-web-builder/internal/builder"
	"github.com/huhen/lampa-web-builder/internal/config"
	"github.com/huhen/lampa-web-builder/internal/execrun"
	"github.com/huhen/lampa-web-builder/internal/gitops"
	"github.com/huhen/lampa-web-builder/internal/pipeline"
	"github.com/huhen/lampa-web-builder/internal/state"
)

// Version is reported by GET /api/v1/status. It is overridden at build time
// with -ldflags "-X main.Version=..."; a plain `go build` keeps "dev".
var Version = "dev"

func main() {
	if err := run(); err != nil {
		log.Fatalf("fatal: %v", err)
	}
}

func run() error {
	cfg, err := config.FromOS()
	if err != nil {
		return err
	}
	store, err := state.Open(cfg.DataDir)
	if err != nil {
		return err
	}
	exe := execrun.ExecRunner{}
	git := gitops.NewClient(cfg.UpstreamRepo, cfg.UpstreamBranch, exe)
	pipe, err := pipeline.New(cfg.AssetsDir, filepath.Join(cfg.DataDir, "deps"), exe)
	if err != nil {
		return err
	}
	b := builder.New(cfg, store, git, pipe)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	go b.RunWorker(ctx)
	go b.RunPoller(ctx)

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           api.New(b, Version),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() {
		errCh <- srv.ListenAndServe()
	}()
	log.Printf("listening on %s (data dir %s, assets %s)", cfg.Listen, cfg.DataDir, cfg.AssetsDir)

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return err
		}
	case <-ctx.Done():
	}
	// In-flight builds die with the process; state recovery marks them failed
	// (reason: restart) on the next start.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
