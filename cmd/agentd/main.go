package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"android-agent/internal/backend"
	"android-agent/internal/cdp"
	"android-agent/internal/config"
	"android-agent/internal/guard"
	"android-agent/internal/mcp"
)

func main() {
	var (
		configPath  = flag.String("config", "/data/adb/android_agent/config.json", "Path to config.json")
		initConfig  = flag.Bool("init-config", true, "Create default config with random token if missing")
		showVersion = flag.Bool("version", false, "Print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("%s %s\n", mcp.ServerName, mcp.ServerVersion)
		return
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	logf := func(format string, args ...any) {
		log.Printf(format, args...)
	}

	cfg, err := config.Load(*configPath, *initConfig)
	if err != nil {
		log.Fatalf("config error: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	be := backend.NewShellBackend(cfg.ClipHelperPath, time.Duration(cfg.ShellTimeoutMs)*time.Millisecond)
	cdpClient := cdp.NewClient(cfg.CDPSocket)

	// Graceful CDP probe on startup (never fails startup if Chrome is not installed)
	cdpSt := cdpClient.Probe(ctx)
	if cdpSt.Available {
		logf("cdp: connected to %s", cdpSt.Socket)
	} else {
		logf("cdp: degraded mode (%s not available; continuing normally)", cdpSt.Socket)
	}

	g := guard.New(guard.Options{
		Packages:      cfg.GuardPackages,
		PollInterval:  time.Duration(cfg.GuardPollIntervalMs) * time.Millisecond,
		ResumeGrace:   time.Duration(cfg.GuardResumeGraceMs) * time.Millisecond,
		PauseFlagPath: cfg.PauseFlagPath,
		Logger:        logf,
	})

	srv := mcp.NewServer(cfg, be, g, cdpClient, logf)
	g.SetListener(srv)

	// Run initial guard check before opening port 8765 so we never bind while a guarded game is already foreground
	g.CheckOnce(ctx)
	if !g.Status().GamePaused {
		if err := srv.Start(); err != nil {
			log.Fatalf("failed to start MCP server: %v", err)
		}
	} else {
		logf("startup: guarded game already in foreground; deferring listener start")
	}

	go g.Run(ctx)

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1, syscall.SIGUSR2)
	for {
		sig := <-sigCh
		switch sig {
		case syscall.SIGUSR1:
			g.PauseManual("SIGUSR1")
		case syscall.SIGUSR2:
			g.ResumeManual()
		case syscall.SIGINT, syscall.SIGTERM:
			logf("received signal %v, shutting down cleanly", sig)
			cancel()
			srv.Close()
			return
		}
	}
}
