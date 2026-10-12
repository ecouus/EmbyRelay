package main

import (
	"context"
	"embyrelay/internal/control"
	"embyrelay/internal/version"
	"embyrelay/web"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

func main() {
	if e := run(); e != nil {
		slog.Error("server stopped", "reason", e.Error())
		os.Exit(1)
	}
}
func run() error {
	dir := os.Getenv("RELAY_DATA_DIR")
	if dir == "" {
		dir = "data"
	}
	if len(os.Args) > 1 {
		if len(os.Args) != 3 || os.Args[1] != "reset-password" {
			return fmt.Errorf("usage: embyrelay reset-password USERNAME (reads RELAY_ADMIN_PASSWORD)")
		}
		dbPath := filepath.Join(dir, "embyrelay.db")
		if os.Getenv("RELAY_DB_DRIVER") != "postgres" {
			if _, err := os.Stat(dbPath); err != nil {
				return fmt.Errorf("existing database required: %w", err)
			}
		}
		store, err := control.OpenConfigured(dir)
		if err != nil {
			return err
		}
		defer store.Close()
		if err := store.ResetAdminPassword(os.Args[2], os.Getenv("RELAY_ADMIN_PASSWORD")); err != nil {
			return err
		}
		slog.Info("administrator password reset; existing sessions revoked; TOTP unchanged")
		return nil
	}
	master, e := control.LoadMaster(dir, os.Getenv("RELAY_MASTER_KEY"))
	if e != nil {
		return e
	}
	store, e := control.OpenConfigured(dir)
	if e != nil {
		return e
	}
	defer store.Close()
	if e = store.Bootstrap(os.Getenv("RELAY_ADMIN_USER"), os.Getenv("RELAY_ADMIN_PASSWORD")); e != nil {
		return e
	}
	app, e := control.New(store, master, os.Getenv("RELAY_ALLOW_INSECURE") != "true", web.Assets())
	if e != nil {
		return e
	}
	addr := os.Getenv("RELAY_LISTEN")
	if addr == "" {
		addr = "127.0.0.1:6689"
	}
	srv := &http.Server{Addr: addr, Handler: app, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 * 1024}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go app.Maintenance(ctx)
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if srv.Shutdown(shutdown) != nil {
			srv.Close()
		}
	}()
	slog.Info("EmbyRelay ready", "address", addr, "version", version.Version)
	if cert, key := os.Getenv("RELAY_TLS_CERT"), os.Getenv("RELAY_TLS_KEY"); cert != "" && key != "" {
		e = srv.ListenAndServeTLS(cert, key)
	} else {
		e = srv.ListenAndServe()
	}
	if e == http.ErrServerClosed {
		return nil
	}
	return e
}
