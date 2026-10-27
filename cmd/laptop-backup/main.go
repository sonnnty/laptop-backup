//go:build linux

// Command laptop-backup mirrors chosen folders into Google Drive and serves
// a small web UI on 127.0.0.1:8095.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"laptop-backup/internal/auth"
	"laptop-backup/internal/config"
	"laptop-backup/internal/drive"
	"laptop-backup/internal/index"
	"laptop-backup/internal/schedule"
	bsync "laptop-backup/internal/sync"
	"laptop-backup/internal/web"
)

const maxLogSize = 5 << 20

func main() {
	addr := flag.String("addr", "127.0.0.1:8095", "listen address (loopback only)")
	initOnly := flag.Bool("init", false, "write the default config if missing, then exit")
	host := flag.String("host", envOr("LAPTOP_BACKUP_HOST", "backup"),
		"host name a local reverse proxy forwards to the UI (empty = loopback only; env LAPTOP_BACKUP_HOST)")
	flag.Parse()

	if runtime.GOOS != "linux" {
		fmt.Fprintln(os.Stderr, "laptop-backup runs on Linux only")
		os.Exit(1)
	}
	if err := run(*addr, *host, *initOnly); err != nil {
		fmt.Fprintln(os.Stderr, "laptop-backup:", err)
		os.Exit(1)
	}
}

func run(addr, host string, initOnly bool) error {
	if err := checkLoopback(addr); err != nil {
		return err
	}
	paths, err := config.DefaultPaths()
	if err != nil {
		return err
	}
	for _, d := range []string{paths.ConfigDir, paths.DataDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return err
		}
	}

	store, err := config.Load(paths.ConfigFile())
	if err != nil {
		return err
	}
	if _, err := os.Stat(paths.ConfigFile()); errors.Is(err, os.ErrNotExist) {
		if err := store.Save(); err != nil {
			return err
		}
		fmt.Println("wrote default config:", paths.ConfigFile())
	}
	if initOnly {
		return nil
	}

	logFile, err := openLog(paths.LogFile())
	if err != nil {
		return err
	}
	defer logFile.Close()
	log := slog.New(slog.NewTextHandler(io.MultiWriter(os.Stderr, logFile), nil))

	idx, err := index.Open(paths.StateDB())
	if err != nil {
		return err
	}
	defer idx.Close()

	authm, err := auth.New(paths.CredentialsFile(), paths.TokenFile())
	if err != nil {
		return err
	}
	newDrive := func(ctx context.Context) (drive.Client, error) {
		hc, err := authm.HTTPClient()
		if err != nil {
			return nil, err
		}
		return drive.New(ctx, hc)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	sched := &schedule.Scheduler{
		Schedule: func() config.Schedule { return store.Get().Schedule },
		LastRuns: func() (time.Time, time.Time) {
			start, success, err := idx.LastRuns()
			if err != nil {
				log.Error("read run history", "err", err)
			}
			return start, success
		},
	}
	runner := &bsync.Runner{
		Index: idx, Config: store, Home: paths.Home, Log: log,
		NewDrive: newDrive, BaseCtx: ctx, OnDone: sched.Reset,
		Fatal: func(err error) bool { return auth.IsInvalidGrant(err) || drive.IsStorageFull(err) },
	}
	sched.Run = func(ctx context.Context) error { return runner.Run(ctx, "schedule", false) }

	srv := &web.Server{
		Config: store, Auth: authm, Runner: runner, Sched: sched, Index: idx,
		Paths: paths, Log: log, NewDrive: newDrive, Hosts: web.LoopbackHosts(addr, host),
	}
	handler, err := srv.Handler()
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
	go func() {
		if err := httpSrv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("http server", "err", err)
			stop()
		}
	}()
	go sched.Loop(ctx)
	log.Info("laptop-backup started", "addr", addr, "config", paths.ConfigFile(), "google", authm.Status().Connected)

	<-ctx.Done()
	log.Info("shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	httpSrv.Shutdown(shutCtx)
	runner.Cancel()
	runner.Wait()
	return nil
}

func envOr(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

// checkLoopback refuses to listen anywhere but on a loopback address.
func checkLoopback(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("refusing to listen on %q: use a loopback IP such as 127.0.0.1", addr)
	}
	return nil
}

// openLog opens the log file for appending, keeping one previous file
// (laptop-backup.log.1) once it grows past maxLogSize.
func openLog(path string) (*os.File, error) {
	if info, err := os.Stat(path); err == nil && info.Size() > maxLogSize {
		os.Rename(path, path+".1")
	}
	return os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
}
