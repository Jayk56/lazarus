package main

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/jayk56/lazarus/internal/api"
	"github.com/jayk56/lazarus/internal/auth"
	"github.com/jayk56/lazarus/internal/store"
)

var version = "dev"

func main() {
	// Group-writable database files let a replacement Kubernetes pod use the
	// volume without needing elevated permissions.
	syscall.Umask(0o007)
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(runHealthcheck(os.Args[2:]))
		case "verify":
			os.Exit(runVerify(os.Args[2:]))
		case "restore":
			os.Exit(runRestore(os.Args[2:]))
		case "version", "--version", "-version":
			fmt.Println(version)
			return
		}
	}
	if err := runServer(); err != nil {
		slog.Error("server_exit", "error", err)
		os.Exit(1)
	}
}

func runServer() error {
	// Start signal handling before opening the database or network listener.
	signalCtx, stop := signalContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	dbPath := env("LAZARUS_DB_PATH", "/var/lib/lazarus/lazarus.db")
	tokenPath := env("LAZARUS_TOKEN_FILE", "/etc/lazarus/tokens")
	backupDir := env("LAZARUS_BACKUP_DIR", filepath.Join(filepath.Dir(dbPath), "backups"))
	keep, err := backupKeepEnv()
	if err != nil {
		return err
	}
	backupMinAge, err := durationEnv("LAZARUS_BACKUP_MIN_AGE", 24*time.Hour)
	if err != nil {
		return err
	}
	st, err := store.Open(signalCtx, dbPath)
	if err != nil {
		return err
	}
	closeStore := true
	defer func() {
		if closeStore {
			_ = st.Close()
		}
	}()
	a, err := auth.LoadFile(tokenPath)
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	srv := api.New(api.Config{Store: st, Auth: a, Version: version, BackupDir: backupDir, BackupKeep: keep, BackupMinAge: backupMinAge, Logger: logger})
	writeTimeout, err := durationEnv("LAZARUS_WRITE_TIMEOUT", 2*time.Minute)
	if err != nil {
		return err
	}
	shutdownTimeout, err := durationEnv("LAZARUS_SHUTDOWN_TIMEOUT", 30*time.Second)
	if err != nil {
		return err
	}
	var activeHandlers sync.WaitGroup
	trackedHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		activeHandlers.Add(1)
		defer activeHandlers.Done()
		srv.Handler().ServeHTTP(w, r)
	})
	httpServer := &http.Server{
		Addr:    env("LAZARUS_ADDR", ":8080"),
		Handler: trackedHandler,
		BaseContext: func(net.Listener) context.Context {
			// SIGTERM cancels active backup, verification, and download requests
			// before the database closes.
			return signalCtx
		},
		TLSConfig:         lazarusTLSConfig(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      writeTimeout,
		IdleTimeout:       120 * time.Second,
	}
	serverErr := make(chan error, 1)
	cert, key := os.Getenv("LAZARUS_TLS_CERT_FILE"), os.Getenv("LAZARUS_TLS_KEY_FILE")
	if (cert == "") != (key == "") {
		return fmt.Errorf("LAZARUS_TLS_CERT_FILE and LAZARUS_TLS_KEY_FILE must be set together")
	}
	go func() {
		if cert != "" {
			serverErr <- httpServer.ListenAndServeTLS(cert, key)
			return
		}
		serverErr <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-serverErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-signalCtx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
		defer cancel()
		srv.SetReady(false)
		shutdownErr := httpServer.Shutdown(shutdownCtx)
		if shutdownErr != nil {
			// After the shutdown deadline, stop connections and give canceled
			// requests a short interval to finish using SQLite.
			_ = httpServer.Close()
		}
		handlersDone := make(chan struct{})
		go func() {
			activeHandlers.Wait()
			close(handlersDone)
		}()
		select {
		case <-handlersDone:
		case <-time.After(5 * time.Second):
			closeStore = false
			return fmt.Errorf("graceful shutdown: handlers did not stop after cancellation")
		}
		if shutdownErr != nil {
			return fmt.Errorf("graceful shutdown: %w", shutdownErr)
		}
		return nil
	}
}

// signalContext is kept in a helper so main remains easy to exercise in tests.
func signalContext(parent context.Context, signals ...os.Signal) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, signals...)
	go func() {
		select {
		case <-ch:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, func() { signal.Stop(ch); cancel() }
}

func env(key, fallback string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}
	return fallback
}

func durationEnv(key string, fallback time.Duration) (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive Go duration", key)
	}
	return parsed, nil
}

func backupKeepEnv() (int, error) {
	const (
		fallback = 7
		minimum  = 1
		maximum  = 1000
	)
	value := strings.TrimSpace(os.Getenv("LAZARUS_BACKUP_KEEP"))
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < minimum || parsed > maximum {
		return 0, fmt.Errorf("LAZARUS_BACKUP_KEEP must be an integer from %d through %d", minimum, maximum)
	}
	return parsed, nil
}

func lazarusTLSConfig() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS12}
}

func runHealthcheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	url := fs.String("url", "http://127.0.0.1:8080/healthz", "health URL")
	insecure := fs.Bool("insecure", false, "skip TLS certificate verification")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	config := lazarusTLSConfig()
	if *insecure {
		config.InsecureSkipVerify = true // explicitly requested by operator
	}
	transport.TLSClientConfig = config
	client := &http.Client{Transport: transport, Timeout: 10 * time.Second}
	resp, err := client.Get(*url)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		fmt.Fprintf(os.Stderr, "healthcheck failed: HTTP %s\n", resp.Status)
		return 1
	}
	return 0
}

func runVerify(args []string) int {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	database := fs.String("database", "", "database path")
	if err := fs.Parse(args); err != nil || *database == "" {
		return 2
	}
	result, err := store.VerifyPath(context.Background(), *database)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	data, _ := json.Marshal(result)
	fmt.Println(string(data))
	return 0
}

func runRestore(args []string) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	database := fs.String("database", "", "destination database path")
	backup := fs.String("backup", "", "validated backup path")
	manifest := fs.String("manifest", "", "backup manifest path")
	replace := fs.Bool("replace", false, "replace an existing database")
	if err := fs.Parse(args); err != nil || *database == "" || *backup == "" || *manifest == "" {
		return 2
	}
	if err := restoreDatabase(*database, *backup, *manifest, *replace); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

func restoreDatabase(database, backup, manifestPath string, replace bool) error {
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var manifest store.BackupManifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return fmt.Errorf("invalid manifest: %w", err)
	}
	if filepath.Base(backup) != manifest.Filename || manifest.IntegrityCheck != "ok" || manifest.ForeignKeys != "ok" || manifest.Application != "ok" {
		return fmt.Errorf("backup does not match a valid manifest")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, err := os.Stat(backup + suffix); err == nil {
			return fmt.Errorf("backup must be a standalone SQLite file; found %s", filepath.Base(backup+suffix))
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	destinationExists := false
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		path := database + suffix
		if info, err := os.Stat(path); err == nil {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("destination SQLite path %s is not a regular file", filepath.Base(path))
			}
			destinationExists = true
		} else if !os.IsNotExist(err) {
			return err
		}
	}
	if destinationExists && !replace {
		return fmt.Errorf("destination SQLite files exist; pass --replace explicitly")
	}
	if err := os.MkdirAll(filepath.Dir(database), 0o770); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(database), ".lazarus-restore-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o660); err != nil {
		_ = tmp.Close()
		return err
	}
	source, err := os.Open(backup)
	if err != nil {
		_ = tmp.Close()
		return err
	}
	sourceInfo, err := source.Stat()
	if err != nil || !sourceInfo.Mode().IsRegular() {
		_ = source.Close()
		_ = tmp.Close()
		if err != nil {
			return err
		}
		return fmt.Errorf("backup must be a regular file")
	}
	hash := sha256.New()
	written, copyErr := io.Copy(io.MultiWriter(tmp, hash), source)
	closeSourceErr := source.Close()
	if copyErr != nil {
		_ = tmp.Close()
		return copyErr
	}
	if closeSourceErr != nil {
		_ = tmp.Close()
		return closeSourceErr
	}
	if written != manifest.Bytes || written != sourceInfo.Size() || !strings.EqualFold(manifest.SHA256, hex.EncodeToString(hash.Sum(nil))) {
		_ = tmp.Close()
		return fmt.Errorf("backup does not match a valid manifest")
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	verified, err := store.VerifyPath(context.Background(), tmpPath)
	if err != nil {
		return fmt.Errorf("backup sqlite validation failed: %w", err)
	}
	if verified.IntegrityCheck != "ok" || verified.ForeignKeys != "ok" || verified.ApplicationData != "ok" {
		return fmt.Errorf("backup sqlite validation failed")
	}

	rollbackBase := ""
	preserved := make([][2]string, 0, 4)
	if destinationExists {
		rollbackBase = fmt.Sprintf("%s.rollback-%s", database, time.Now().UTC().Format("20060102T150405.000000000Z"))
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			from := database + suffix
			to := rollbackBase + suffix
			if _, err := os.Stat(from); err == nil {
				if err := os.Rename(from, to); err != nil {
					for i := len(preserved) - 1; i >= 0; i-- {
						_ = os.Rename(preserved[i][1], preserved[i][0])
					}
					return fmt.Errorf("preserve current SQLite file %s: %w", filepath.Base(from), err)
				}
				preserved = append(preserved, [2]string{from, to})
			} else if !os.IsNotExist(err) {
				return err
			}
		}
	}
	if err := os.Rename(tmpPath, database); err != nil {
		for i := len(preserved) - 1; i >= 0; i-- {
			_ = os.Rename(preserved[i][1], preserved[i][0])
		}
		return err
	}
	if err := syncDirectory(filepath.Dir(database)); err != nil {
		return fmt.Errorf("sync restore directory: %w", err)
	}
	if rollbackBase != "" {
		fmt.Fprintf(os.Stderr, "saved the replaced database as %s\n", rollbackBase)
	}
	return nil
}

func syncDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
