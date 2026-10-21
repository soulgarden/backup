package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/soulgarden/backup/internal/backup"
)

var version = "dev"

type exitError int

func (exitError) Error() string { return "command failed" }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	os.Exit(run(ctx, os.Args[1:], os.Stdout))
}

func run(ctx context.Context, args []string, output io.Writer) int {
	cmd := newCommand(viper.New())
	cmd.SetArgs(args)
	cmd.SetOut(output)
	cmd.SetErr(output)

	err := cmd.ExecuteContext(ctx)
	if err == nil {
		return 0
	}

	var status exitError
	if errors.As(err, &status) {
		return int(status)
	}

	// Parser errors can contain user input. Log only a fixed, non-secret code.
	backup.Emit(output, "backup_failed", "preflight", "", "INVALID_ARGUMENTS", "ERROR")
	return 1
}

func newCommand(settings *viper.Viper) *cobra.Command {
	root := &cobra.Command{
		Use:           "backup",
		Short:         "Create and verify a PostgreSQL archive in S3",
		Version:       version,
		Args:          cobra.NoArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return execute(cmd, settings)
		},
	}

	flags := root.PersistentFlags()
	flags.String("config", "/config/config.json", "configuration JSON")
	flags.String("credentials", "/credentials", "credentials directory")
	flags.String("workspace", "/work", "temporary workspace parent")
	_ = settings.BindPFlags(flags)
	_ = settings.BindEnv("config", "BACKUP_CONFIG")
	_ = settings.BindEnv("credentials", "S3_CREDENTIALS_DIR")
	_ = settings.BindEnv("workspace", "BACKUP_WORKSPACE")

	contract := &cobra.Command{
		Use:   "contract",
		Short: "Verify a dedicated DigitalOcean fixture bucket",
		Args:  cobra.NoArgs,
		RunE:  root.RunE,
	}
	contract.Flags().String("canary-bucket", "", "separate provisioned fixture canary bucket")
	contract.Flags().String("scope-evidence", "", "sanitized limited-key scope evidence")
	contract.Flags().String("report", "/work/provider.json", "non-secret report path")
	_ = contract.MarkFlagRequired("canary-bucket")
	_ = contract.MarkFlagRequired("scope-evidence")

	lifecycle := &cobra.Command{
		Use:   "verify-lifecycle",
		Short: "Read back the required multipart cleanup rule",
		Args:  cobra.NoArgs,
		RunE:  root.RunE,
	}
	lifecycle.Flags().String("report", "/work/lifecycle.json", "non-secret report path")

	root.AddCommand(contract, lifecycle)
	return root
}

func execute(cmd *cobra.Command, settings *viper.Viper) error {
	output := cmd.OutOrStdout()
	runID, err := backup.NewRunID(time.Now())
	if err != nil {
		backup.Emit(output, "backup_failed", "preflight", "", "INVALID_SETUP", "ERROR")
		return exitError(1)
	}

	fail := func(code string) error {
		backup.Emit(output, "backup_failed", "preflight", runID, code, "ERROR")
		if cmd.Context().Err() != nil {
			return exitError(143)
		}
		return exitError(1)
	}

	c, err := backup.LoadConfig(settings.GetString("config"))
	if err != nil {
		return fail("INVALID_SETUP")
	}

	ctx := cmd.Context()
	grace := time.Duration(c.Process.ShutdownGraceSeconds) * time.Second
	done := make(chan struct{})
	defer close(done)

	// A hard watchdog covers stuck child/kernel/transport cleanup even as PID1.
	go func() {
		select {
		case <-ctx.Done():
			select {
			case <-done:
			case <-time.After(grace):
				os.Exit(143)
			}
		case <-done:
		}
	}()

	creds, err := backup.LoadCredentials(settings.GetString("credentials"))
	if err != nil {
		return fail("INVALID_SETUP")
	}
	store := backup.NewS3(c.S3, creds)

	switch cmd.Name() {
	case "backup":
		dir, err := os.MkdirTemp(settings.GetString("workspace"), "backup-")
		if err != nil {
			return fail("INVALID_SETUP")
		}
		defer os.RemoveAll(dir)

		runner := backup.Runner{
			Config:    c,
			Processes: backup.Processes{Grace: grace},
			Store:     store,
			Version:   version,
			Output:    output,
		}
		if status := runner.Run(ctx, dir, runID); status != 0 {
			return exitError(status)
		}

	case "contract":
		canary, _ := cmd.Flags().GetString("canary-bucket")
		evidence, _ := cmd.Flags().GetString("scope-evidence")
		report, _ := cmd.Flags().GetString("report")
		err = backup.ProviderContract(ctx, store, canary, evidence, report)

	case "verify-lifecycle":
		report, _ := cmd.Flags().GetString("report")
		err = backup.VerifyLifecycle(ctx, store, report)
	}

	if err != nil {
		return fail("CONTRACT_FAILED")
	}
	return nil
}
