package backup

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

type testHTTPDoer func(*http.Request) (*http.Response, error)

func (f testHTTPDoer) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestClientMatrix(t *testing.T) {
	if os.Getenv("BACKUP_CLIENT_MATRIX") != "1" {
		t.Skip("real tool matrix")
	}

	c := fixtureConfig(t)
	p := Processes{time.Second}
	for _, tool := range []string{"pg_dump", "pg_restore"} {
		t.Run(tool, func(t *testing.T) {
			selected := fakeCommand{func(ctx context.Context, a, e []string, w io.Writer) (string, error) {
				if a[0] == tool {
					return p.Run(ctx, a, e, w)
				}

				return a[0] + " (PostgreSQL) 16.1", nil
			}}
			if _, err := ReadVersions(context.Background(), selected, c, fixtureInventory()); err == nil ||
				err.Error() != "CLIENT_MAJOR_MISMATCH" {
				t.Fatal("incompatible client accepted", err)
			}
		})
	}
}

func TestFixture(t *testing.T) {
	if os.Getenv("BACKUP_INTEGRATION") != "1" {
		t.Skip("disposable PG16/S3 fixtures")
	}

	ctx := context.Background()
	c := fixtureConfig(t)
	c.S3.Endpoint = "http://moto:5000"
	c.S3.AddressingStyle = "path"
	c.S3.MultipartThresholdMiB = 5
	c.S3.MultipartChunkSizeMiB = 5
	c.Backup.IncludeSchemas = []string{"public", "work schema"}
	c.Backup.ExcludeSchemas = []string{"release_archive_*"}
	c.Validation = Validation{
		[]string{"public", "work schema"},
		[]RequiredTable{{"public", "partition root", true}},
		3,
	}

	c.Retention.Enabled = true
	store := NewS3(c.S3, Credentials{"fixture", "fixture", ""})
	if _, err := store.Client.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(c.S3.Bucket)}); err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	processes := Processes{5 * time.Second}
	runID := "20261020T233000Z-" + strings.Repeat("a", 32)
	runner := Runner{c, processes, store, "0.2.0", os.Stdout, time.Now}

	for _, tc := range []struct {
		name    string
		options string
	}{
		{"startup options discarded", ""},
		{"read-only discarded", "-c statement_timeout=5000"},
		{"timeout discarded", "-c default_transaction_read_only=on"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dumpStarted := false
			stripped := runner
			stripped.Processes = fakeCommand{
				func(ctx context.Context, args, env []string, out io.Writer) (string, error) {
					if args[0] == "pg_dump" {
						dumpStarted = true
					}

					var filtered []string
					for _, value := range env {
						if !strings.HasPrefix(value, "PGOPTIONS=") {
							filtered = append(filtered, value)
						}
					}

					filtered = append(filtered, "PGOPTIONS="+tc.options)
					return processes.Run(ctx, args, filtered, out)
				},
			}
			id, err := NewRunID(time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if status := stripped.Run(ctx, work, id); status != 1 || dumpStarted {
				t.Fatalf(
					"unsafe session was not rejected before dump: status=%d dump=%t",
					status,
					dumpStarted,
				)
			}
			objects, err := store.List(ctx, c.S3.Prefix+id+"/")
			if err != nil || len(objects) != 0 {
				t.Fatal("unsafe session published objects", err)
			}
		})
	}

	if code := runner.Run(ctx, work, runID); code != 0 {
		t.Fatal("runner", code)
	}

	m, err := store.ReadManifest(ctx, c.S3.Prefix+runID+"/manifest.json")
	if err != nil || m.DatabaseRestoreTested || m.ArchiveBytes <= 32<<20 {
		t.Fatal(m, err)
	}

	env, err := PGEnv(c, true)
	if err != nil {
		t.Fatal(err)
	}

	toc, err := processes.Run(
		ctx,
		[]string{"pg_restore", "--list", "--verbose", filepath.Join(work, "backup.dump")},
		nil,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}

	for _, kind := range []string{"SEQUENCE", " INDEX ", "FK CONSTRAINT"} {
		if !strings.Contains(toc, kind) {
			t.Fatal("missing fixture", kind)
		}
	}

	if strings.Contains(toc, "release_archive_fixture") {
		t.Fatal("excluded schema present")
	}

	for _, include := range []bool{true, false} {
		options := c
		options.Backup = c.Backup
		options.Backup.IncludeSchemas = []string{}
		options.Backup.IncludeACL = include
		options.Backup.IncludeLargeObjects = include
		path := filepath.Join(work, "options.dump")
		if _, err = processes.Run(ctx, DumpArgs(options, path), env, nil); err != nil {
			t.Fatal(err)
		}

		text, e := processes.Run(ctx, []string{"pg_restore", "--list", "--verbose", path}, nil, nil)
		if e != nil {
			t.Fatal(e)
		}

		hasBlob := strings.Contains(text, " BLOB ") || strings.Contains(text, " BLOBS ")
		if hasBlob != include || strings.Contains(text, " ACL ") != include {
			t.Fatal("options not applied")
		}

		if _, e = processes.Run(ctx, []string{"pg_restore", "--file=/dev/null", path}, nil, nil); e != nil {
			t.Fatal(e)
		}
	}

	excluded := c
	excluded.Backup.ExcludeTableData = []string{`public."leaf one"`}
	path := filepath.Join(work, "excluded.dump")
	if _, err = processes.Run(ctx, DumpArgs(excluded, path), env, nil); err != nil {
		t.Fatal(err)
	}

	text, err := processes.Run(ctx, []string{"pg_restore", "--list", "--verbose", path}, nil, nil)
	if err != nil {
		t.Fatal(err)
	}

	inv, err := ReadInventory(ctx, processes, env)
	if err != nil {
		t.Fatal(err)
	}

	if _, err = ValidateTOC(text, inv, inv, c.Validation); err == nil {
		t.Fatal("missing leaf data accepted")
	}

	corrupt := filepath.Join(work, "corrupt.dump")
	if _, err = processes.Run(ctx, DumpArgs(c, corrupt), env, nil); err != nil {
		t.Fatal(err)
	}

	st, err := os.Stat(corrupt)
	if err != nil {
		t.Fatal(err)
	}

	if err = os.Truncate(corrupt, st.Size()-200); err != nil {
		t.Fatal(err)
	}

	if _, err = processes.Run(ctx, []string{"pg_restore", "--list", corrupt}, nil, nil); err != nil {
		t.Fatal(err)
	}

	if _, err = processes.Run(ctx, []string{"pg_restore", "--file=/dev/null", corrupt}, nil, nil); err == nil {
		t.Fatal("truncated archive accepted")
	}

	parts := filepath.Join(work, "parts")
	f, err := os.Create(parts)
	if err != nil {
		t.Fatal(err)
	}

	_, err = io.CopyN(f, rand.Reader, 96<<20)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}

	if err = store.Upload(ctx, parts, "fixture/multipart"); err != nil {
		t.Fatal(err)
	}

	size, sha, err := HashFile(parts)
	if err != nil {
		t.Fatal(err)
	}

	if err = store.Verify(ctx, "fixture/multipart", size, sha); err != nil {
		t.Fatal(err)
	}
	// Cancel after one real part is stored, while the next part is blocked.
	cancelledConfig := c.S3
	cancelledConfig.MaxConcurrency = 1
	cancelledStore := NewS3(cancelledConfig, Credentials{"fixture", "fixture", ""})
	options := cancelledStore.Client.Options()
	baseHTTP := options.HTTPClient
	entered := make(chan struct{})
	options.HTTPClient = testHTTPDoer(func(request *http.Request) (*http.Response, error) {
		if request.URL.Query().Get("partNumber") == "2" {
			close(entered)
			<-request.Context().Done()
			return nil, request.Context().Err()
		}

		return baseHTTP.Do(request)
	})
	cancelledStore.Client = s3.New(options)
	uploadCtx, cancelUpload := context.WithCancel(ctx)
	go func() {
		select {
		case <-entered:
			cancelUpload()
		case <-uploadCtx.Done():
		}
	}()
	cancelStarted := time.Now()
	err = cancelledStore.Upload(uploadCtx, parts, "fixture/cancelled")
	cancelUpload()
	if err == nil || time.Since(cancelStarted) > 10*time.Second {
		t.Fatal("cancel/abort failed", err)
	}

	mpu, e := store.Client.ListMultipartUploads(
		ctx,
		&s3.ListMultipartUploadsInput{Bucket: aws.String(c.S3.Bucket)},
	)
	if e != nil || len(mpu.Uploads) != 0 {
		t.Fatal("MPU parts left after cancelled upload", e)
	}

	for n := 0; n < 1001; n++ {
		_, err = store.Client.PutObject(
			ctx,
			&s3.PutObjectInput{
				Bucket: aws.String(c.S3.Bucket),
				Key:    aws.String("pagination/" + time.Unix(int64(n), 0).Format(time.RFC3339)),
				Body:   strings.NewReader("x"),
			},
		)
		if err != nil {
			t.Fatal(err)
		}
	}

	objects, err := store.List(ctx, "pagination/")
	if err != nil || len(objects) != 1001 {
		t.Fatal(len(objects), err)
	}

	for _, partial := range []bool{false, true} {
		broken := fakeCommand{func(ctx context.Context, args, env []string, w io.Writer) (string, error) {
			if args[0] == "pg_dump" && args[1] != "--version" {
				if partial {
					for _, v := range args {
						if strings.HasPrefix(v, "--file=") {
							_ = os.WriteFile(strings.TrimPrefix(v, "--file="), []byte("partial"), 0600)
						}
					}
				}

				return "", io.ErrUnexpectedEOF
			}

			return processes.Run(ctx, args, env, w)
		}}
		r := runner
		r.Processes = broken
		id := "20261020T233000Z-" + strings.Repeat("b", 32)
		if partial {
			id = "20261020T233000Z-" + strings.Repeat("c", 32)
		}

		if r.Run(ctx, work, id) == 0 {
			t.Fatal("dump failure accepted")
		}

		objects, err = store.List(ctx, c.S3.Prefix+id+"/")
		if err != nil || len(objects) != 0 {
			t.Fatal("premature upload")
		}
	}

	var peakDisk int64
	peakEntries, err := os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}

	for _, entry := range peakEntries {
		info, e := entry.Info()
		if e != nil {
			t.Fatal(e)
		}

		peakDisk += info.Size()
	}

	t.Setenv("PGDATABASE", "fixture_empty")
	empty := c
	empty.Backup.IncludeSchemas = []string{}
	empty.Backup.ExcludeSchemas = []string{}
	empty.Validation = Validation{[]string{}, []RequiredTable{}, 0}
	empty.Retention.Enabled = false
	empty.S3.Prefix = "empty/v2/"
	emptyRunner := runner
	emptyRunner.Config = empty
	if emptyRunner.Run(ctx, work, "20261020T233000Z-"+strings.Repeat("d", 32)) != 0 {
		t.Fatal("empty generic")
	}

	empty.Validation = c.Validation
	empty.S3.Prefix = "empty-rejected/v2/"
	emptyRunner.Config = empty
	if emptyRunner.Run(ctx, work, "20261020T233000Z-"+strings.Repeat("e", 32)) == 0 {
		t.Fatal("empty required accepted")
	}

	entries, err := os.ReadDir(work)
	if err != nil {
		t.Fatal(err)
	}

	var disk int64
	for _, entry := range entries {
		info, e := entry.Info()
		if e != nil {
			t.Fatal(e)
		}

		disk += info.Size()
	}

	if disk < peakDisk {
		disk = peakDisk
	}

	var usage syscall.Rusage
	if err = syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		t.Fatal(err)
	}

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	report := map[string]any{
		"archive_bytes":         m.ArchiveBytes,
		"stream_object_bytes":   96 << 20,
		"workspace_peak_bytes":  disk,
		"peak_rss_kib":          usage.Maxrss,
		"go_heap_sys_bytes":     mem.HeapSys,
		"sql_restore_executed":  false,
		"uid":                   os.Getuid(),
		"multipart":             true,
		"cancelled_mpu_aborted": true,
		"pagination":            1001,
	}

	if reportPath := os.Getenv("INTEGRATION_REPORT"); reportPath != "" {
		if err = writeReport(reportPath, report); err != nil {
			t.Fatal(err)
		}
	}

	raw, _ := json.Marshal(report)
	t.Log(string(raw))
}
