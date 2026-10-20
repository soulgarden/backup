package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/soulgarden/backup/internal/backup"
)

var version = "dev"

func env(name, fallback string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return fallback
}
func main() { os.Exit(run()) }
func run() int {
	configPath := flag.String("config", env("BACKUP_CONFIG", "/config/config.json"), "configuration JSON")
	credentialsPath := flag.String("credentials", env("S3_CREDENTIALS_DIR", "/credentials"), "credentials directory")
	workspace := flag.String("workspace", env("BACKUP_WORKSPACE", "/work"), "temporary workspace parent")
	mode := flag.String("mode", "backup", "backup, contract or verify-lifecycle")
	canary := flag.String("canary-bucket", "", "separate provisioned fixture canary bucket")
	evidence := flag.String("scope-evidence", "", "sanitized limited-key scope evidence")
	report := flag.String("report", "/work/provider.json", "non-secret report path")
	flag.Parse()
	runID, err := backup.NewRunID(time.Now())
	if err != nil {
		return 1
	}
	c, err := backup.LoadConfig(*configPath)
	if err != nil {
		backup.Emit(os.Stdout, "backup_failed", "preflight", runID, "INVALID_SETUP", "ERROR")
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	// A hard watchdog covers stuck child/kernel/transport cleanup even as PID1.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			select {
			case <-done:
			case <-time.After(time.Duration(c.Process.ShutdownGraceSeconds) * time.Second):
				os.Exit(143)
			}
		case <-done:
		}
	}()
	creds, err := backup.LoadCredentials(*credentialsPath)
	if err != nil {
		backup.Emit(os.Stdout, "backup_failed", "preflight", runID, "INVALID_SETUP", "ERROR")
		return 1
	}
	store := backup.NewS3(c.S3, creds)
	switch *mode {
	case "backup":
		dir, err := os.MkdirTemp(*workspace, "backup-")
		if err != nil {
			return 1
		}
		defer os.RemoveAll(dir)
		return (backup.Runner{Config: c, Processes: backup.Processes{Grace: time.Duration(c.Process.ShutdownGraceSeconds) * time.Second}, Store: store, Version: version, Output: os.Stdout}).Run(ctx, dir, runID)
	case "contract":
		err = backup.ProviderContract(ctx, store, *canary, *evidence, *report)
	case "verify-lifecycle":
		err = backup.VerifyLifecycle(ctx, store, *report)
	default:
		return 1
	}
	if err != nil {
		backup.Emit(os.Stdout, "backup_failed", "preflight", runID, "CONTRACT_FAILED", "ERROR")
		return 1
	}
	return 0
}
