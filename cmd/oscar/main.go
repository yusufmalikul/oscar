// Command oscar runs the Oscar server: one Captain session, workers and reviewers,
// and a web UI to watch them and approve what they want to do.
//
//	oscar            run under a small supervisor (restarts after self-update, rolls back a bad one)
//	oscar serve      run the server directly
//	oscar check      load config, task types and prompts, then exit (used before installing an update)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"oscar/internal/config"
	"oscar/internal/engine"
	"oscar/internal/store"
	"oscar/internal/web"
)

func main() {
	cmd := "supervise"
	args := os.Args[1:]
	if len(args) > 0 && (args[0] == "serve" || args[0] == "check" || args[0] == "supervise") {
		cmd, args = args[0], args[1:]
	}
	home, _ := os.UserHomeDir()
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	cfgPath := fs.String("config", filepath.Join(home, ".oscar", "config.yaml"), "config file")
	_ = fs.Parse(args)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	switch cmd {
	case "check":
		if _, err := engine.New(context.Background(), cfg); err != nil {
			log.Fatalf("check: %v", err)
		}
		fmt.Println("ok")
	case "serve":
		os.Exit(serve(cfg))
	default:
		os.Exit(supervise(cfg, *cfgPath))
	}
}

func serve(cfg config.Config) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	e, err := engine.New(ctx, cfg)
	if err != nil {
		log.Printf("engine: %v", err)
		return 1
	}
	srv := &http.Server{Addr: cfg.Listen, Handler: web.New(e, web.Static()).Handler(), ReadHeaderTimeout: 10 * time.Second}
	code := 0
	restart := make(chan struct{}, 1)
	e.Restart = func() {
		select {
		case restart <- struct{}{}:
		default:
		}
	}
	e.Start()
	go func() {
		select {
		case <-ctx.Done():
		case <-restart:
			code = engine.ExitRestart
		}
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	fmt.Printf("Oscar is running on http://%s (data in %s)\n", cfg.Listen, cfg.DataDir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Print(err)
		e.Shutdown()
		return 1
	}
	e.Shutdown()
	return code
}

// supervise runs `oscar serve` as a child. Exit code ExitRestart means "start again" (after a
// self-update). If the child crashes soon after an update, the previous binary is restored.
func supervise(cfg config.Config, cfgPath string) int {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		log.Printf("supervisor: %v", err)
		return 1
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	var updatedAt time.Time
	var crashes []time.Time
	for {
		child := exec.Command(exe, "serve", "-config", cfgPath)
		child.Stdout, child.Stderr = os.Stdout, os.Stderr
		child.Env = append(os.Environ(), "OSCAR_SUPERVISED=1")
		started := time.Now()
		if err := child.Start(); err != nil {
			log.Printf("supervisor: start: %v", err)
			return 1
		}
		done := make(chan error, 1)
		go func() { done <- child.Wait() }()
		var werr error
		stopping := false
		select {
		case s := <-sig:
			stopping = true
			_ = child.Process.Signal(s)
			werr = <-done
		case werr = <-done:
		}
		if stopping {
			return 0
		}
		code := 0
		var ee *exec.ExitError
		if errors.As(werr, &ee) {
			code = ee.ExitCode()
		} else if werr != nil {
			code = 1
		}
		switch {
		case code == 0:
			return 0
		case code == engine.ExitRestart:
			log.Print("supervisor: restarting into the new version")
			updatedAt = time.Now()
			continue
		}
		prev := exe + ".prev"
		if !updatedAt.IsZero() && time.Since(started) < time.Minute {
			if _, err := os.Stat(prev); err == nil {
				log.Printf("supervisor: new version exited with %d right after an update; rolling back", code)
				if err := os.Rename(prev, exe); err != nil {
					log.Printf("supervisor: rollback failed: %v", err)
					return 1
				}
				if st, err := store.Open(cfg.DataDir); err == nil {
					_ = st.SetValue("update-rollback.json", map[string]string{"detail": fmt.Sprintf("exit code %d", code), "at": time.Now().Format(time.RFC3339)})
				}
				updatedAt = time.Time{}
				continue
			}
		}
		updatedAt = time.Time{}
		now := time.Now()
		crashes = append(crashes, now)
		for len(crashes) > 0 && now.Sub(crashes[0]) > 10*time.Minute {
			crashes = crashes[1:]
		}
		if len(crashes) > 5 {
			log.Printf("supervisor: Oscar crashed %d times in 10 minutes; giving up", len(crashes))
			return 1
		}
		log.Printf("supervisor: Oscar exited with %d; restarting in 3s", code)
		time.Sleep(3 * time.Second)
	}
}
