package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"

	"android-agent/internal/backend"
	"android-agent/internal/cdp"
	"android-agent/internal/config"
	"android-agent/internal/guard"
	"android-agent/internal/mcp"
)

const maxLogFileBytes = 2 * 1024 * 1024

type rotatingFileWriter struct {
	mu       sync.Mutex
	path     string
	maxBytes int64
	f        *os.File
	size     int64
}

func newRotatingFileWriter(path string, maxBytes int64) (*rotatingFileWriter, error) {
	if maxBytes <= 0 {
		maxBytes = maxLogFileBytes
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	var sz int64
	if st, err := f.Stat(); err == nil {
		sz = st.Size()
	}
	return &rotatingFileWriter{
		path:     path,
		maxBytes: maxBytes,
		f:        f,
		size:     sz,
	}, nil
}

func (w *rotatingFileWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return 0, os.ErrClosed
	}
	if w.size+int64(len(p)) > w.maxBytes && w.size > 0 {
		_ = w.f.Close()
		_ = os.Rename(w.path, w.path+".1")
		nf, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			w.f = nil
			return 0, err
		}
		w.f = nf
		w.size = 0
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingFileWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f == nil {
		return nil
	}
	err := w.f.Close()
	w.f = nil
	return err
}

func main() {
	var (
		configPath  = flag.String("config", "/data/adb/android_agent/config.json", "Path to config.json")
		initConfig  = flag.Bool("init-config", true, "Create default config with random token if missing")
		logFile     = flag.String("log-file", "", "Optional log file path with 2MB in-process rotation")
		showVersion = flag.Bool("version", false, "Print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Printf("%s %s\n", mcp.ServerName, mcp.ServerVersion)
		return
	}

	tzMsg := initAndroidTimezone()

	if strings.TrimSpace(*logFile) != "" {
		rw, err := newRotatingFileWriter(strings.TrimSpace(*logFile), maxLogFileBytes)
		if err != nil {
			log.Printf("log rotation init failed (%v); falling back to stderr", err)
		} else {
			log.SetOutput(rw)
			defer rw.Close()
		}
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	logf := func(format string, args ...any) {
		log.Printf(format, args...)
	}
	logf("%s", tzMsg)
	initAndroidCACerts(logf)

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
	srv.SetConfigPath(*configPath)
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
	go srv.StartScheduler(ctx)

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
			be.Close()
			return
		}
	}
}

func initAndroidTimezone() string {
	tz := strings.TrimSpace(os.Getenv("TZ"))
	if tz == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
		defer cancel()
		if out, err := exec.CommandContext(ctx, "/system/bin/getprop", "persist.sys.timezone").Output(); err == nil {
			tz = strings.TrimSpace(string(out))
		}
	}
	if tz == "" {
		tz = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		if sh, shErr := time.LoadLocation("Asia/Shanghai"); shErr == nil {
			time.Local = sh
			return fmt.Sprintf("tz: failed to load %q (%v); fell back to Asia/Shanghai", tz, err)
		}
		return fmt.Sprintf("tz: failed to load %q (%v); using %s", tz, err, time.Local.String())
	}
	time.Local = loc
	return fmt.Sprintf("tz: using %s (%s)", tz, time.Now().Format("-07:00"))
}

// initAndroidCACerts loads Android's Conscrypt/system root CA certificates into
// http.DefaultTransport when running as a static linux/arm64 binary on Android.
func initAndroidCACerts(logf func(string, ...any)) {
	candidates := []string{
		"/apex/com.android.conscrypt/cacerts",
		"/system/etc/security/cacerts",
		"/data/misc/user/0/cacerts-added",
	}
	var existingDirs []string
	var systemLoaded bool
	var certFiles []string

	for _, dir := range candidates {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		existingDirs = append(existingDirs, dir)
		isSystemDir := dir != "/data/misc/user/0/cacerts-added"
		if isSystemDir && systemLoaded {
			continue
		}
		dirCount := 0
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			certFiles = append(certFiles, filepath.Join(dir, e.Name()))
			dirCount++
		}
		if isSystemDir && dirCount > 0 {
			systemLoaded = true
		}
	}

	if len(existingDirs) == 0 {
		return
	}
	if os.Getenv("SSL_CERT_DIR") == "" {
		_ = os.Setenv("SSL_CERT_DIR", strings.Join(existingDirs, ":"))
	}

	pool, err := x509.SystemCertPool()
	if err != nil || pool == nil {
		pool = x509.NewCertPool()
	}
	loaded := 0
	for _, p := range certFiles {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if pool.AppendCertsFromPEM(b) {
			loaded++
		}
	}
	if loaded > 0 {
		if tr, ok := http.DefaultTransport.(*http.Transport); ok {
			if tr.TLSClientConfig == nil {
				tr.TLSClientConfig = &tls.Config{}
			}
			tr.TLSClientConfig.RootCAs = pool
		}
		logf("tls: loaded %d Android root CAs", loaded)
	}
}
