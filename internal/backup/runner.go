package backup

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"time"
)

type Runner struct {
	Config    Config
	Processes Command
	Store     Store
	Version   string
	Output    io.Writer
	Now       func() time.Time
}

func NewRunID(now time.Time) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}

	return now.UTC().Format("20060102T150405Z-") + hex.EncodeToString(b), nil
}

func Emit(w io.Writer, event, stage, runID, code, level string) {
	_ = json.NewEncoder(w).Encode(struct {
		Level string `json:"level"`
		Event string `json:"event"`
		Stage string `json:"stage"`
		Code  string `json:"code"`
		RunID string `json:"run_id"`
	}{level, event, stage, code, runID})
}

func (r Runner) Run(ctx context.Context, workspace, runID string) int {
	if r.Output == nil {
		r.Output = io.Discard
	}

	if r.Now == nil {
		r.Now = time.Now
	}

	stage := "preflight"
	manifest, err := func() (Manifest, error) {
		c := r.Config
		env, err := PGEnv(c, true)
		if err != nil {
			return Manifest{}, err
		}

		inv, err := ReadInventory(ctx, r.Processes, env)
		if err != nil {
			return Manifest{}, err
		}

		versions, err := ReadVersions(ctx, r.Processes, c, inv)
		if err != nil {
			return Manifest{}, err
		}

		profile := struct {
			Backup               Filters
			Validation           Validation
			PostgreSQL           PostgreSQL
			Database, Host, User string
		}{c.Backup, c.Validation, c.PostgreSQL, os.Getenv("PGDATABASE"), os.Getenv("PGHOST"), os.Getenv("PGUSER")}
		raw, _ := json.Marshal(profile)
		fingerprint := sha256.Sum256(raw)
		stage = "dump"
		started := r.Now().UTC().Format(time.RFC3339Nano)
		part := filepath.Join(workspace, "backup.dump.part")
		dumpEnv, err := PGEnv(c, false)
		if err != nil {
			return Manifest{}, err
		}

		if _, err = r.Processes.Run(ctx, DumpArgs(c, part), dumpEnv, nil); err != nil {
			return Manifest{}, err
		}

		finished := r.Now().UTC().Format(time.RFC3339Nano)
		stage = "local_validation"
		toc, err := os.Create(filepath.Join(workspace, "toc.txt"))
		if err != nil {
			return Manifest{}, err
		}

		_, err = r.Processes.Run(ctx, []string{"pg_restore", "--list", "--verbose", part}, nil, toc)
		closeErr := toc.Close()
		if err != nil {
			return Manifest{}, err
		}

		if closeErr != nil {
			return Manifest{}, closeErr
		}

		if _, err = r.Processes.Run(ctx, []string{"pg_restore", "--file=/dev/null", part}, nil, nil); err != nil {
			return Manifest{}, err
		}

		after, err := ReadInventory(ctx, r.Processes, env)
		if err != nil {
			return Manifest{}, err
		}

		text, err := os.ReadFile(filepath.Join(workspace, "toc.txt"))
		if err != nil {
			return Manifest{}, err
		}

		result, err := ValidateTOC(string(text), inv, after, c.Validation)
		if err != nil {
			return Manifest{}, err
		}

		path := filepath.Join(workspace, "backup.dump")
		if err = os.Rename(part, path); err != nil {
			return Manifest{}, err
		}

		size, sha, err := HashFile(path)
		if err != nil {
			return Manifest{}, err
		}

		if size == 0 {
			return Manifest{}, errors.New("EMPTY_ARCHIVE")
		}

		key := c.S3.Prefix + runID + "/backup.dump"
		stage = "upload"
		if err = r.Store.Upload(ctx, path, key); err != nil {
			return Manifest{}, err
		}

		stage = "remote_validation"
		if err = r.Store.Verify(ctx, key, size, sha); err != nil {
			return Manifest{}, err
		}

		m := Manifest{
			FormatVersion:            2,
			RunID:                    runID,
			Database:                 os.Getenv("PGDATABASE"),
			ArchiveKey:               key,
			ArchiveBytes:             size,
			ArchiveSHA256:            sha,
			DumpStartedAt:            started,
			DumpFinishedAt:           finished,
			VerificationFinishedAt:   r.Now().UTC().Format(time.RFC3339Nano),
			ImageVersion:             r.Version,
			DumpMajor:                c.PostgreSQL.MajorVersion,
			RequiredRestoreMajor:     c.PostgreSQL.MajorVersion,
			TargetServerMajor:        c.PostgreSQL.TargetServerMajor,
			ConfigurationFingerprint: hex.EncodeToString(fingerprint[:]),
			Filters:                  c.Backup,
			Checks:                   Checks{result, true, true},
			VerificationScope:        VerificationScope,
			Status:                   "completed",
			Versions:                 versions,
		}

		stage = "manifest"
		if err = r.Store.Publish(ctx, c.S3.Prefix+runID+"/manifest.json", m); err != nil {
			return Manifest{}, err
		}

		return m, nil
	}()
	if err != nil {
		code := "STAGE_FAILED"
		exit := 1
		if ctx.Err() != nil {
			code = "CANCELLED"
			exit = 143
		}

		Emit(r.Output, "backup_failed", stage, runID, code, "ERROR")
		return exit
	}

	Emit(r.Output, "backup_completed", "manifest", runID, "OK", "INFO")
	err = func() error {
		complete, _, err := ListRuns(ctx, r.Store, r.Config.S3.Prefix)
		if err != nil {
			return err
		}

		if SizeAnomaly(complete, manifest, r.Config.Observability.SizeAnomaly) {
			Emit(r.Output, "backup_size_anomaly", "retention", runID, "SIZE_CHANGED", "ERROR")
		}

		plan, err := Rotate(ctx, r.Store, r.Config, runID, r.Now().UTC())
		if err == nil {
			_ = json.NewEncoder(r.Output).Encode(struct {
				Level      string      `json:"level"`
				Event      string      `json:"event"`
				Candidates []Candidate `json:"candidates"`
			}{"INFO", "backup_retention_plan", plan})
		}

		return err
	}()
	if ctx.Err() != nil {
		return 143
	}

	if err != nil {
		Emit(r.Output, "backup_retention_failed", "retention", runID, "CLEANUP_FAILED", "ERROR")
	}

	return 0
}
