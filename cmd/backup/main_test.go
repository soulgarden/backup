package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/soulgarden/backup/internal/backup"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
)

func TestContractShutdownBudget(t *testing.T) {
	if shutdownGrace("contract", 5) <= backup.ContractCleanupTimeout {
		t.Fatal("contract watchdog interrupts cleanup")
	}
	for _, command := range []string{"backup", "verify-lifecycle"} {
		if shutdownGrace(command, 45) != 45*time.Second {
			t.Fatal("normal shutdown budget changed")
		}
	}
}

func TestPathPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  bool
		args []string
		want []string
	}{
		{
			name: "defaults",
			want: []string{"/config/config.json", "/credentials", "/work"},
		},
		{
			name: "environment",
			env:  true,
			want: []string{"env-config", "env-credentials", "env-workspace"},
		},
		{
			name: "flags override environment",
			env:  true,
			args: []string{
				"--config", "flag-config",
				"--credentials", "flag-credentials",
				"--workspace", "flag-workspace",
			},
			want: []string{"flag-config", "flag-credentials", "flag-workspace"},
		},
		{
			name: "subcommand inherits paths",
			env:  true,
			args: []string{"verify-lifecycle", "--config", "flag-config"},
			want: []string{"flag-config", "env-credentials", "env-workspace"},
		},
		{
			name: "contract uses configured paths",
			args: []string{"contract"},
			want: []string{"/config/config.json", "/credentials", "/work"},
		},
		{
			name: "contract dry run",
			args: []string{"contract", "--dry-run"},
			want: []string{"/config/config.json", "/credentials", "/work"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for key, value := range map[string]string{
				"BACKUP_CONFIG": "env-config", "S3_CREDENTIALS_DIR": "env-credentials",
				"BACKUP_WORKSPACE": "env-workspace",
			} {
				if !tc.env {
					value = ""
				}
				t.Setenv(key, value)
			}

			settings := viper.New()
			cmd := newCommand(settings)
			capture := func(_ *cobra.Command, _ []string) error {
				for i, key := range []string{"config", "credentials", "workspace"} {
					if got := settings.GetString(key); got != tc.want[i] {
						t.Fatalf("%s = %q, want %q", key, got, tc.want[i])
					}
				}
				return nil
			}
			cmd.RunE = capture
			for _, sub := range cmd.Commands() {
				sub.RunE = capture
			}
			cmd.SetArgs(tc.args)
			cmd.SetOut(io.Discard)
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHelpAndVersion(t *testing.T) {
	t.Setenv("BACKUP_CONFIG", "/missing/config.json")
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--help"}, "Create and verify a PostgreSQL archive"},
		{[]string{"--version"}, version},
		{[]string{"contract", "--help"}, "--dry-run"},
		{[]string{"verify-lifecycle", "--help"}, "--report"},
	} {
		var output bytes.Buffer
		if status := run(context.Background(), tc.args, &output); status != 0 {
			t.Fatalf("%v: exit %d", tc.args, status)
		}
		if !strings.Contains(output.String(), tc.want) {
			t.Fatalf("%v: %s", tc.args, output.String())
		}
	}
}

func TestInvalidArgumentsAreSanitized(t *testing.T) {
	for _, args := range [][]string{
		{"--unknown=secret-value"},
		{"secret-value"},
		{"contract", "secret-value"},
		{"contract", "--dry-run=secret-value"},
		{"verify-lifecycle", "secret-value"},
		{"--config"},
	} {
		var output bytes.Buffer
		if status := run(context.Background(), args, &output); status != 1 {
			t.Fatalf("%v: exit %d", args, status)
		}
		assertFailure(t, output.Bytes(), "INVALID_ARGUMENTS")
	}
}

func TestSetupAndContractFailures(t *testing.T) {
	dir := t.TempDir()
	config := filepath.Join(dir, "config.json")
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(config, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"access_key", "secret_key"} {
		if err = os.WriteFile(filepath.Join(dir, key), []byte("secret-value"), 0600); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("PGHOST", "")
	for _, tc := range []struct {
		args []string
		code string
	}{
		{[]string{"--config", filepath.Join(dir, "secret-value")}, "INVALID_SETUP"},
		{[]string{"--credentials", filepath.Join(dir, "secret-value")}, "INVALID_SETUP"},
		{[]string{"--workspace", filepath.Join(dir, "secret-value")}, "INVALID_SETUP"},
		{nil, "STAGE_FAILED"},
		{
			[]string{"contract", "--report", filepath.Join(dir, "missing", "report.json")},
			"CONTRACT_FAILED",
		},
	} {
		var output bytes.Buffer
		args := append([]string{"--config", config, "--credentials", dir, "--workspace", dir}, tc.args...)
		if status := run(context.Background(), args, &output); status != 1 {
			t.Fatalf("%v: exit %d", tc.args, status)
		}
		assertFailure(t, output.Bytes(), tc.code)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var output bytes.Buffer
	if status := run(ctx, []string{"--config", config, "--credentials", dir, "--workspace", dir}, &output); status != 143 {
		t.Fatalf("cancelled command: exit %d", status)
	}
	assertFailure(t, output.Bytes(), "CANCELLED")
}

func assertFailure(t *testing.T, output []byte, code string) {
	t.Helper()
	var event struct {
		Level string `json:"level"`
		Event string `json:"event"`
		Code  string `json:"code"`
	}
	if err := json.Unmarshal(output, &event); err != nil {
		t.Fatalf("expected one JSON error: %s", output)
	}
	if event.Level != "ERROR" || event.Event != "backup_failed" || event.Code != code {
		t.Fatalf("unexpected event: %s", output)
	}
	if strings.Contains(string(output), "secret-value") {
		t.Fatalf("input leaked: %s", output)
	}
}
