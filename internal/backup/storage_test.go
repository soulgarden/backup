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
					body = bytes.ReplaceAll(raw, []byte(`"database_restore_tested":false`), []byte(`"database_restore_tested":0`))
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
					_, _ = io.WriteString(w, `<ListBucketResult><IsTruncated>true</IsTruncated></ListBucketResult>`)
					return
				}
				truncated := page == 1 || mode == "repeated_token"
				fmt.Fprintf(w, `<ListBucketResult><IsTruncated>%t</IsTruncated><NextContinuationToken>next</NextContinuationToken><Contents><Key>a/file%d</Key><Size>4</Size><LastModified>2026-10-20T00:00:00Z</LastModified></Contents></ListBucketResult>`, truncated, page)
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
		_, _ = io.WriteString(w, `<VersioningConfiguration><Status>Suspended</Status></VersioningConfiguration>`)
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
					_, _ = io.WriteString(w, `<InitiateMultipartUploadResult><UploadId>fixture</UploadId></InitiateMultipartUploadResult>`)
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
					_, _ = io.WriteString(w, `<CompleteMultipartUploadResult><ETag>multipart</ETag></CompleteMultipartUploadResult>`)
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
	s := testS3(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) })
	if ProviderContract(context.Background(), s, "backup-fixture-canary", "missing", "missing") == nil {
		t.Fatal("production fixture accepted")
	}
	s.Config.Bucket = "backup-fixture-test"
	if ProviderContract(context.Background(), s, s.Config.Bucket, "missing", "missing") == nil {
		t.Fatal("same bucket")
	}
	path := filepath.Join(t.TempDir(), "scope")
	_ = os.WriteFile(path, []byte(`{"bucket":"backup-fixture-test","access":"Read/Write/Delete","limited_access":false}`), 0600)
	if ProviderContract(context.Background(), s, "backup-fixture-canary", path, "missing") == nil {
		t.Fatal("wide key")
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
				fmt.Fprintf(w, `<LifecycleConfiguration><Rule><ID>abort</ID><Status>%s</Status><Filter><Prefix>%s</Prefix>%s</Filter><AbortIncompleteMultipartUpload><DaysAfterInitiation>1</DaysAfterInitiation></AbortIncompleteMultipartUpload></Rule></LifecycleConfiguration>`, status, prefix, tag)
			})
			err := VerifyLifecycle(context.Background(), store, filepath.Join(t.TempDir(), "report"))
			if (err == nil) != (mode == "valid") {
				t.Fatal(mode, err)
			}
		})
	}
}

func TestProviderContractHTTP(t *testing.T) {
	var mu sync.Mutex
	objects := map[string][]byte{}
	parts := map[string]map[int][]byte{}
	uploads := map[string]string{}
	next := 0
	s := testS3(t, func(w http.ResponseWriter, r *http.Request) {
		fields := strings.SplitN(strings.TrimPrefix(r.URL.Path, "/"), "/", 2)
		bucket := fields[0]
		key := ""
		if len(fields) == 2 {
			key = fields[1]
		}
		q := r.URL.Query()
		if bucket == "backup-fixture-canary" {
			w.Header().Set("Content-Type", "application/xml")
			w.WriteHeader(403)
			_, _ = io.WriteString(w, `<Error><Code>AccessDenied</Code></Error>`)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/xml")
		switch {
		case q.Has("versioning"):
			_, _ = io.WriteString(w, `<VersioningConfiguration/>`)
		case q.Has("list-type"):
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
				fmt.Fprintf(w, "<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-10-20T00:00:00Z</LastModified></Contents>", k, len(objects[k]))
			}
			fmt.Fprint(w, `</ListBucketResult>`)
		case r.Method == "POST" && q.Has("uploads"):
			next++
			id := strconv.Itoa(next)
			parts[id] = map[int][]byte{}
			uploads[id] = key
			fmt.Fprintf(w, `<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>`, id)
		case r.Method == "PUT" && q.Has("partNumber"):
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
			fmt.Fprint(w, `<CompleteMultipartUploadResult><ETag>multipart</ETag></CompleteMultipartUploadResult>`)
		case r.Method == "DELETE" && q.Has("uploadId"):
			delete(parts, q.Get("uploadId"))
			delete(uploads, q.Get("uploadId"))
			w.WriteHeader(204)
		case r.Method == "GET" && q.Has("uploads"):
			fmt.Fprint(w, `<ListMultipartUploadsResult/>`)
		case r.Method == "PUT":
			objects[key], _ = io.ReadAll(r.Body)
			w.Header().Set("ETag", `"single"`)
		case r.Method == "DELETE":
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
	s.Config.Bucket = "backup-fixture-contract"
	dir := t.TempDir()
	evidence := filepath.Join(dir, "scope.json")
	if err := os.WriteFile(evidence, []byte(`{"bucket":"backup-fixture-contract","limited_access":true,"access":"Read/Write/Delete"}`), 0600); err != nil {
		t.Fatal(err)
	}
	report := filepath.Join(dir, "report.json")
	if err := ProviderContract(context.Background(), s, "backup-fixture-canary", evidence, report); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(report)
	if err != nil || !bytes.Contains(raw, []byte(`"fixture_cleanup": true`)) {
		t.Fatal(string(raw), err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(objects) != 0 || len(parts) != 0 || len(uploads) != 0 {
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
			fmt.Fprintf(w, `<ListBucketResult><IsTruncated>false</IsTruncated><Contents>%s</Contents></ListBucketResult>`, contents)
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
