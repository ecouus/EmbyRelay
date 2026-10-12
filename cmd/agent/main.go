package main

import (
	"context"
	"embyrelay/internal/control"
	"embyrelay/internal/relay"
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
		slog.Error("agent stopped", "reason", e.Error())
		os.Exit(1)
	}
}
func run() error {
	dir := os.Getenv("RELAY_DATA_DIR")
	if dir == "" {
		dir = "agent-data"
	}
	master, e := control.LoadMaster(dir, os.Getenv("RELAY_MASTER_KEY"))
	if e != nil {
		return e
	}
	store, e := control.Open(filepath.Join(dir, "agent.db"))
	if e != nil {
		return e
	}
	defer store.Close()
	app, e := control.New(store, master, true, nil)
	if e != nil {
		return e
	}
	agent, e := relay.New(app, store, dir, os.Getenv("RELAY_PUBLIC_URL"), os.Getenv("RELAY_CONTROL_URL"), os.Getenv("RELAY_NODE_NAME"), os.Getenv("RELAY_ENROLL_TOKEN"), os.Getenv("RELAY_SERVER_KEY"), os.Getenv("RELAY_ALLOW_INSECURE") == "true")
	if e != nil {
		return e
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go agent.Run(ctx)
	go app.Maintenance(ctx)
	addr := os.Getenv("RELAY_LISTEN")
	if addr == "" {
		addr = "127.0.0.1:8081"
	}
	srv := &http.Server{Addr: addr, Handler: app, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 * 1024}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if srv.Shutdown(shutdown) != nil {
			srv.Close()
		}
	}()
	slog.Info("relay agent ready", "address", addr)
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
