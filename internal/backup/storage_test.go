package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func testS3(t *testing.T, handler http.HandlerFunc) *S3Store {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	c := fixtureConfig(t).S3
	c.Endpoint = server.URL
	c.AddressingStyle = "path"
	c.TotalAttempts = 1
	c.MultipartThresholdMiB = 5
	c.MultipartChunkSizeMiB = 5
	return NewS3(c, Credentials{"fixture", "fixture", ""})
}

func TestS3VerifyAndManifest(t *testing.T) {
	data := []byte("data")
	h := sha256.Sum256(data)
	sha := hex.EncodeToString(h[:])
	ctx := context.Background()
	for _, mode := range []string{"ok", "head", "short", "long", "hash", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "HEAD" {
					n := 4
					if mode == "head" {
						n = 5
					}

					w.Header().Set("Content-Length", fmt.Sprint(n))
					return
				}

				body := data
				switch mode {
				case "short":
					body = []byte("da")
				case "long":
					body = []byte("dataextra")
				case "hash":
					body = []byte("evil")
				}

				_, _ = w.Write(body)
			})
			c, cancel := context.WithCancel(ctx)
			defer cancel()
			if mode == "cancel" {
				cancel()
			}

			err := s.Verify(c, "key", 4, sha)
			if (err == nil) != (mode == "ok") {
				t.Fatal(err)
			}
		})
	}

	for _, mode := range []string{"match", "mismatch", "duplicate", "wrong_bool", "huge", "uncertain", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			c := fixtureConfig(t)
			s := newMemory()
			m := addRun(t, s, c, 1, time.Now())
			raw, _ := json.Marshal(m)
			client := testS3(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "PUT" {
					if mode == "uncertain" {
						w.WriteHeader(500)
						return
					}

					_, _ = io.Copy(io.Discard, r.Body)
					return
				}

				if mode == "huge" {
					w.Header().Set("Content-Length", "1048577")
					return
				}

				body := raw
				if mode == "mismatch" {
					body = bytes.ReplaceAll(raw, []byte("fixture"), []byte("other"))
				}

				if mode == "duplicate" {
					body = append([]byte(`{"format_version":2,`), raw[1:]...)
				}

				if mode == "wrong_bool" {
					body = bytes.ReplaceAll(
						raw,
						[]byte(`"database_restore_tested":false`),
						[]byte(`"database_restore_tested":0`),
					)
				}

				_, _ = w.Write(body)
			})
			request, cancel := context.WithCancel(ctx)
			defer cancel()
			if mode == "cancel" {
				cancel()
			}

			err := client.Publish(request, "marker", m)
			if (err == nil) != (mode == "match" || mode == "uncertain") {
				t.Fatal(mode, err)
			}
		})
	}
}

func TestS3Operations(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"missing", "denied", "present"} {
		t.Run(mode, func(t *testing.T) {
			s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
				switch mode {
				case "missing":
					w.WriteHeader(404)
				case "denied":
					w.WriteHeader(403)
				default:
					w.Header().Set("Content-Length", "4")
				}
			})
			absent, err := s.Absent(ctx, "key")
			if absent != (mode == "missing") || (err != nil) != (mode == "denied") {
				t.Fatal(absent, err)
			}
		})
	}

	for _, mode := range []string{"pages", "missing_token", "repeated_token", "second_failure"} {
		t.Run(mode, func(t *testing.T) {
			page := 0
			s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
				page++
				w.Header().Set("Content-Type", "application/xml")
				if page > 1 && mode == "second_failure" {
					w.WriteHeader(403)
					return
				}

				if mode == "missing_token" {
					_, _ = io.WriteString(
						w,
						`<ListBucketResult><IsTruncated>true</IsTruncated></ListBucketResult>`,
					)
					return
				}

				truncated := page == 1 || mode == "repeated_token"
				fmt.Fprintf(
					w,
					`<ListBucketResult><IsTruncated>%t</IsTruncated><NextContinuationToken>next</NextContinuationToken><Contents><Key>a/file%d</Key><Size>4</Size><LastModified>2026-10-20T00:00:00Z</LastModified></Contents></ListBucketResult>`,
					truncated,
					page,
				)
			})
			objects, err := s.List(ctx, "a/")
			if mode == "pages" {
				if err != nil || len(objects) != 2 {
					t.Fatal(objects, err)
				}
			} else if err == nil {
				t.Fatal("partial listing accepted")
			}
		})
	}

	s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(204)
			return
		}

		_, _ = io.WriteString(
			w,
			`<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>`,
		)
	})
	if err := s.Delete(ctx, "key"); err != nil {
		t.Fatal(err)
	}

	if version, err := s.Versioning(ctx); err != nil || version != "Suspended" {
		t.Fatal(version, err)
	}
}

func TestS3UploadAbort(t *testing.T) {
	for _, mode := range []string{"single", "multipart", "part_failure", "complete_failure", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			aborted := false
			started := make(chan struct{})
			once := sync.Once{}
			s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
				q := r.URL.Query()
				w.Header().Set("Content-Type", "application/xml")
				switch {
				case r.Method == "POST" && q.Has("uploads"):
					_, _ = io.WriteString(
						w,
						`<InitiateMultipartUploadResult><UploadId>fixture</UploadId></InitiateMultipartUploadResult>`,
					)
				case r.Method == "PUT" && q.Has("partNumber"):
					once.Do(func() { close(started) })
					if mode == "cancel" {
						select {
						case <-r.Context().Done():
						case <-time.After(time.Second):
						}

						return
					}

					if mode == "part_failure" {
						w.WriteHeader(403)
						return
					}

					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("ETag", `"part"`)
				case r.Method == "POST" && q.Has("uploadId"):
					if mode == "complete_failure" {
						w.WriteHeader(403)
						return
					}

					_, _ = io.WriteString(
						w,
						`<CompleteMultipartUploadResult><ETag>multipart</ETag></CompleteMultipartUploadResult>`,
					)
				case r.Method == "DELETE":
					mu.Lock()
					aborted = true
					mu.Unlock()
					w.WriteHeader(204)
				default:
					_, _ = io.Copy(io.Discard, r.Body)
					w.Header().Set("ETag", `"single"`)
				}
			})
			path := filepath.Join(t.TempDir(), "file")
			size := 100
			if mode != "single" {
				size = 6 << 20
			}

			if err := os.WriteFile(path, make([]byte, size), 0600); err != nil {
				t.Fatal(err)
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				go func() { <-started; cancel() }()
			}

			err := s.Upload(ctx, path, "archive")
			failed := mode == "part_failure" || mode == "complete_failure" || mode == "cancel"
			if (err != nil) != failed {
				t.Fatal(mode, err)
			}

			mu.Lock()
			got := aborted
			mu.Unlock()
			if got != failed {
				t.Fatal("abort missing", mode, got)
			}
		})
	}
}

func TestProviderGuards(t *testing.T) {
	report := filepath.Join(t.TempDir(), "report.json")
	for _, status := range []string{"denied", "Enabled", "Suspended"} {
		t.Run("versioning "+status, func(t *testing.T) {
			writes := 0
			store := testS3(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes++
				}
				if status == "denied" {
					w.WriteHeader(403)
					return
				}
				fmt.Fprintf(w, "<VersioningConfiguration><Status>%s</Status></VersioningConfiguration>", status)
			})
			if ProviderContract(context.Background(), store, ContractOptions{}, report) == nil || writes != 0 {
				t.Fatal("unconfirmed or enabled versioning accepted")
			}
		})
	}
	for _, prefix := range []string{"", "_backup-contract/", "_backup-contract/nested/"} {
		t.Run("overlapping prefix "+prefix, func(t *testing.T) {
			writes := 0
			store := testS3(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method != "GET" {
					writes++
				}
				fmt.Fprint(w, "<VersioningConfiguration/>")
			})
			store.Config.Prefix = prefix
			if ProviderContract(context.Background(), store, ContractOptions{}, report) == nil || writes != 0 {
				t.Fatal("overlapping namespace accepted")
			}
		})
	}

	for _, mode := range []string{"valid", "wrong_prefix", "disabled", "tags"} {
		t.Run(mode, func(t *testing.T) {
			store := testS3(t, func(w http.ResponseWriter, r *http.Request) {
				prefix := "example/v2/"
				status := "Enabled"
				tag := ""
				if mode == "wrong_prefix" {
					prefix = "other/"
				}

				if mode == "disabled" {
					status = "Disabled"
				}

				if mode == "tags" {
					tag = "<Tag><Key>a</Key><Value>b</Value></Tag>"
				}

				fmt.Fprintf(
					w,
					`<LifecycleConfiguration><Rule><ID>abort</ID><Status>%s</Status><Filter><Prefix>%s</Prefix>%s</Filter><AbortIncompleteMultipartUpload><DaysAfterInitiation>1</DaysAfterInitiation></AbortIncompleteMultipartUpload></Rule></LifecycleConfiguration>`,
					status,
					prefix,
					tag,
				)
			})
			err := VerifyLifecycle(context.Background(), store, filepath.Join(t.TempDir(), "report"))
			if (err == nil) != (mode == "valid") {
				t.Fatal(mode, err)
			}
		})
	}
}

func TestProviderContractDryRun(t *testing.T) {
	requests := 0
	store := testS3(t, func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != "GET" || !r.URL.Query().Has("versioning") || r.URL.Path != "/example-backups" {
			t.Error("dry run made a request other than reading configured bucket versioning")
		}
		fmt.Fprint(w, "<VersioningConfiguration/>")
	})
	report := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(report, []byte(`{"status":"completed"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ProviderContract(context.Background(), store, ContractOptions{DryRun: true}, report); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(report)
	var result struct {
		Status  string `json:"status"`
		Bucket  string `json:"bucket"`
		Objects int    `json:"planned_object_count"`
	}
	if err != nil || json.Unmarshal(raw, &result) != nil || result.Status != "dry_run" ||
		result.Bucket != store.Config.Bucket || result.Objects != 1003 || requests != 1 {
		t.Fatal("dry run must describe planned checks without claiming success", string(raw), err)
	}
}

func TestProviderContractHTTP(t *testing.T) {
	for _, mode := range []string{"success", "high_threshold", "foreign_object", "put_failure", "cancelled_put", "abort_failure"} {
		t.Run(mode, func(t *testing.T) { testProviderContractHTTP(t, mode) })
	}
}

func testProviderContractHTTP(t *testing.T, mode string) {
	dir := t.TempDir()
	report := filepath.Join(dir, "report.json")
	if err := os.WriteFile(report, []byte(`{"status":"completed","cleanup_complete":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	objects := map[string][]byte{
		"example/v2/existing/backup.dump": []byte("existing archive"),
		"example/old.sql.gz":              []byte("legacy archive"),
	}
	parts := map[string]map[int][]byte{}
	uploads := map[string]string{}
	next := 0
	multipartParts := 0
	foreignKey := ""
	s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
		fields := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
		bucket := fields[0]
		if bucket != "example-backups" {
			t.Error("request to a bucket other than the configured bucket")
		}
		key := ""
		if len(fields) == 2 {
			key = fields[1]
		}

		q := r.URL.Query()
		raw, readErr := os.ReadFile(report)
		var journal struct {
			Status string   `json:"status"`
			Keys   []string `json:"owned_keys"`
		}
		if readErr != nil || json.Unmarshal(raw, &journal) != nil || journal.Status == "completed" {
			t.Error("stale or unreadable report before provider request")
		}
		if r.Method == "PUT" || r.Method == "POST" || r.Method == "DELETE" {
			if journal.Status != "running" || len(journal.Keys) != 1003 {
				t.Error("recovery journal missing before write")
			}
		}
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case q.Has("versioning"):
			_, _ = io.WriteString(w, `<VersioningConfiguration/>`)
		case q.Has("list-type"):
			if mode == "foreign_object" && foreignKey == "" {
				foreignKey = q.Get("prefix") + "not-owned"
				objects[foreignKey] = []byte("not created by the contract")
			}
			keys := []string{}
			for k := range objects {
				if strings.HasPrefix(k, q.Get("prefix")) {
					keys = append(keys, k)
				}
			}

			sort.Strings(keys)
			start, _ := strconv.Atoi(q.Get("continuation-token"))
			end := min(start+1000, len(keys))
			fmt.Fprint(w, `<ListBucketResult>`)
			fmt.Fprintf(w, "<IsTruncated>%t</IsTruncated>", end < len(keys))
			if end < len(keys) {
				fmt.Fprintf(w, "<NextContinuationToken>%d</NextContinuationToken>", end)
			}

			for _, k := range keys[start:end] {
				fmt.Fprintf(
					w,
					"<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-10-20T00:00:00Z</LastModified></Contents>",
					k,
					len(objects[k]),
				)
			}

			fmt.Fprint(w, `</ListBucketResult>`)
		case r.Method == "POST" && q.Has("uploads"):
			next++
			id := strconv.Itoa(next)
			parts[id] = map[int][]byte{}
			uploads[id] = key
			fmt.Fprintf(
				w,
				`<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>`,
				id,
			)
		case r.Method == "PUT" && q.Has("partNumber"):
			if strings.HasSuffix(key, "/multipart") {
				multipartParts++
			}
			raw, _ := io.ReadAll(r.Body)
			number, _ := strconv.Atoi(q.Get("partNumber"))
			parts[q.Get("uploadId")][number] = raw
			w.Header().Set("ETag", `"part"`)
		case r.Method == "POST" && q.Has("uploadId"):
			id := q.Get("uploadId")
			numbers := []int{}
			for n := range parts[id] {
				numbers = append(numbers, n)
			}

			sort.Ints(numbers)
			var joined bytes.Buffer
			for _, n := range numbers {
				joined.Write(parts[id][n])
			}

			objects[key] = joined.Bytes()
			delete(parts, id)
			delete(uploads, id)
			fmt.Fprint(
				w,
				`<CompleteMultipartUploadResult><ETag>multipart</ETag></CompleteMultipartUploadResult>`,
			)
		case r.Method == "DELETE" && q.Has("uploadId"):
			if mode == "abort_failure" && strings.HasSuffix(key, "/abort") {
				w.WriteHeader(403)
				fmt.Fprint(w, "<Error><Code>AccessDenied</Code></Error>")
				return
			}
			delete(parts, q.Get("uploadId"))
			delete(uploads, q.Get("uploadId"))
			w.WriteHeader(204)
		case r.Method == "GET" && q.Has("uploads"):
			fmt.Fprint(w, "<ListMultipartUploadsResult>")
			for id, key := range uploads {
				if strings.HasPrefix(key, q.Get("prefix")) {
					fmt.Fprintf(w, "<Upload><Key>%s</Key><UploadId>%s</UploadId></Upload>", key, id)
				}
			}
			fmt.Fprint(w, "</ListMultipartUploadsResult>")
		case r.Method == "PUT":
			if mode == "cancelled_put" && strings.Contains(key, "/pages/") {
				objects[key], _ = io.ReadAll(r.Body)
				cancel()
				w.WriteHeader(200)
				return
			}
			if mode == "put_failure" && strings.Contains(key, "/pages/") {
				w.WriteHeader(403)
				fmt.Fprint(w, "<Error><Code>AccessDenied</Code></Error>")
				return
			}
			objects[key], _ = io.ReadAll(r.Body)
			w.Header().Set("ETag", `"single"`)
		case r.Method == "DELETE":
			if mode == "cancelled_put" {
				time.Sleep(time.Millisecond)
			}
			delete(objects, key)
			w.WriteHeader(204)
		default:
			body, ok := objects[key]
			if !ok {
				w.WriteHeader(404)
				return
			}

			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.Itoa(len(body)))
			if r.Method != "HEAD" {
				_, _ = w.Write(body)
			}
		}
	})
	if mode == "high_threshold" {
		s.Config.MultipartThresholdMiB = 128
		s.Config.MultipartChunkSizeMiB = 128
	}
	original := s.Config
	err := ProviderContract(ctx, s, ContractOptions{}, report)
	if s.Config != original {
		t.Fatal("probe settings changed the backup configuration")
	}
	failure := mode == "foreign_object" || mode == "put_failure" || mode == "cancelled_put" || mode == "abort_failure"
	if (err != nil) != failure {
		t.Fatal(err)
	}

	if !failure {
		if multipartParts < 2 {
			t.Fatal("multipart check did not upload multiple parts")
		}
		raw, err := os.ReadFile(report)
		if err != nil || !bytes.Contains(raw, []byte(`"cleanup_complete": true`)) {
			t.Fatal(string(raw), err)
		}
		var result map[string]any
		if json.Unmarshal(raw, &result) != nil || result["status"] != "completed" ||
			result["bucket"] != "example-backups" {
			t.Fatal("incorrect completion report", string(raw))
		}
	} else {
		raw, readErr := os.ReadFile(report)
		var result struct {
			Status     string   `json:"status"`
			Keys       []string `json:"owned_keys"`
			Cleanup    bool     `json:"cleanup_complete"`
			AbortID    string   `json:"abort_upload_id"`
			Operation  string   `json:"failed_operation"`
			HTTPStatus int      `json:"http_status"`
		}
		if readErr != nil || json.Unmarshal(raw, &result) != nil ||
			result.Status != "failed" || len(result.Keys) != 1003 || result.Cleanup != (mode != "abort_failure") {
			t.Fatal("missing recovery journal", string(raw), readErr)
		}
		if mode == "abort_failure" && result.AbortID == "" {
			t.Fatal("missing incomplete upload ID")
		}
		if (mode == "put_failure" || mode == "abort_failure") &&
			(result.HTTPStatus != 403 || result.Operation == "") {
			t.Fatal("missing sanitized failure category")
		}
	}

	mu.Lock()
	defer mu.Unlock()
	expected := 2
	if foreignKey != "" {
		expected++
		if string(objects[foreignKey]) != "not created by the contract" {
			t.Fatal("foreign object modified")
		}
	}
	if string(objects["example/v2/existing/backup.dump"]) != "existing archive" ||
		string(objects["example/old.sql.gz"]) != "legacy archive" {
		t.Fatal("existing objects modified")
	}
	expectedUploads := 0
	if mode == "abort_failure" {
		expectedUploads = 1
	}
	if len(objects) != expected || len(parts) != expectedUploads || len(uploads) != expectedUploads {
		t.Fatal("fixture leak")
	}
}

func TestGETRetryResetsHash(t *testing.T) {
	gets := 0
	s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4")
		if r.Method == "HEAD" {
			return
		}

		gets++
		if gets == 1 {
			_, _ = io.WriteString(w, "da")
			return
		}

		_, _ = io.WriteString(w, "data")
	})
	s.Config.TotalAttempts = 2
	hash := sha256.Sum256([]byte("data"))
	if err := s.Verify(context.Background(), "key", 4, hex.EncodeToString(hash[:])); err != nil {
		t.Fatal(err)
	}

	if gets != 2 {
		t.Fatal(gets)
	}
}

func TestStorageFailures(t *testing.T) {
	ctx := context.Background()
	denied := testS3(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code></Error>`)
	})
	if _, err := denied.Versioning(ctx); err == nil {
		t.Fatal("versioning denied accepted")
	}

	if err := denied.Verify(ctx, "x", 4, "hash"); err == nil {
		t.Fatal("HEAD denied")
	}

	if _, err := denied.ReadManifest(ctx, "x"); err == nil {
		t.Fatal("GET denied")
	}

	if err := denied.Upload(ctx, "/missing", "x"); err == nil {
		t.Fatal("file missing")
	}

	file := filepath.Join(t.TempDir(), "parts")
	if err := os.WriteFile(file, make([]byte, 6<<20), 0600); err != nil {
		t.Fatal(err)
	}

	if err := denied.Upload(ctx, file, "x"); err == nil {
		t.Fatal("create MPU denied")
	}

	if err := denied.Publish(ctx, "x", Manifest{}); err == nil {
		t.Fatal("invalid manifest accepted")
	}

	for _, raw := range []string{"{}", "{invalid"} {
		s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Length", "30")
			_, _ = io.WriteString(w, raw)
		})
		if _, err := s.ReadManifest(ctx, "x"); err == nil {
			t.Fatal("truncated manifest")
		}
	}

	file = filepath.Join(t.TempDir(), "single")
	_ = os.WriteFile(file, []byte("data"), 0600)
	count := 0
	s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "HEAD" {
			w.Header().Set("Content-Length", "4")
			return
		}

		count++
		w.WriteHeader(403)
	})
	if err := s.Verify(ctx, "key", 4, "hash"); err == nil || count != 1 {
		t.Fatal("GET failure", err, count)
	}
}

func TestMalformedListingNeverDeletes(t *testing.T) {
	for _, contents := range []string{
		`<Key>example/v2/file</Key><Size>4</Size>`,
		`<Key>example/v2/file</Key><LastModified>2026-10-20T00:00:00Z</LastModified>`,
		`<Key>example/v2/file</Key><Size>-1</Size><LastModified>2026-10-20T00:00:00Z</LastModified>`,
		`<Size>4</Size><LastModified>2026-10-20T00:00:00Z</LastModified>`,
	} {
		deletes := 0
		s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "DELETE" {
				deletes++
				w.WriteHeader(204)
				return
			}

			w.Header().Set("Content-Type", "application/xml")
			if r.URL.Query().Has("versioning") {
				fmt.Fprint(w, `<VersioningConfiguration/>`)
				return
			}

			fmt.Fprintf(
				w,
				`<ListBucketResult><IsTruncated>false</IsTruncated><Contents>%s</Contents></ListBucketResult>`,
				contents,
			)
		})
		c := fixtureConfig(t)
		c.Retention.Enabled = true
		c.Retention.DryRun = false
		c.Retention.DeletionApproved = true
		if _, err := Rotate(context.Background(), s, c, "unused", time.Now()); err == nil {
			t.Fatal("malformed listing accepted")
		}

		if deletes > 0 {
			t.Fatal("unsafe delete")
		}
	}
}
