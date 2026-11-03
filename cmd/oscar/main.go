// Command oscar runs the Oscar server: one Captain session, workers and reviewers,
// and a web UI to watch them and approve what they want to do.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"oscar/internal/config"
	"oscar/internal/engine"
	"oscar/internal/web"
)

func main() {
	home, _ := os.UserHomeDir()
	cfgPath := flag.String("config", filepath.Join(home, ".oscar", "config.yaml"), "config file")
	flag.Parse()

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	e, err := engine.New(ctx, cfg)
	if err != nil {
		log.Fatalf("engine: %v", err)
	}
	e.Start()

	srv := &http.Server{Addr: cfg.Listen, Handler: web.New(e, web.Static()).Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	fmt.Printf("Oscar is running on http://%s (data in %s)\n", cfg.Listen, cfg.DataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
	e.Shutdown()
}
