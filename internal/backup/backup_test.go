package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fixtureConfig(t *testing.T) Config {
	t.Helper()
	path := os.Getenv("CONFIG_FIXTURE")
	if path == "" {
		path = "../../config.example.json"
	}

	c, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}

	return c
}

func pgEnvironment(t *testing.T) {
	t.Helper()
	for _, key := range []string{"PGHOST", "PGUSER", "PGDATABASE", "PGPASSWORD"} {
		t.Setenv(key, "fixture")
	}
}

func fixtureInventory() Inventory {
	return Inventory{
		"16.3",
		160003,
		[]Schema{{2200, "public"}, {50, "odd schema"}},
		[]Table{
			{100, "public", "root", "p", false, true, []uint32{}},
			{101, "public", "child", "r", false, true, []uint32{100}},
		},
	}
}

const fixtureTOC = "1; 2615 2200 SCHEMA - public owner\n2; 2615 50 SCHEMA - odd schema owner\n3; 1259 100 TABLE public root owner\n4; 1259 101 TABLE public child owner\n5; 0 101 TABLE DATA public child owner\n"

func TestConfig(t *testing.T) {
	c := fixtureConfig(t)
	raw, err := os.ReadFile("../../config.example.json")
	if err != nil {
		t.Fatal(err)
	}

	var expected Config
	if err = json.Unmarshal(raw, &expected); err != nil {
		t.Fatal(err)
	}

	if !reflect.DeepEqual(c, expected) {
		t.Fatal("Viper changed explicit configuration values")
	}

	data, _ := json.Marshal(c)
	if _, err := ParseConfig(data); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		change func(map[string]any)
	}{
		{"unknown", func(m map[string]any) { m["unexpected"] = true }},
		{"old_s3", func(m map[string]any) { m["s3"].(map[string]any)["host_bucket"] = "old" }},
		{"disable_sha", func(m map[string]any) { m["s3"].(map[string]any)["verifyRemoteSha256"] = false }},
		{"missing_bool", func(m map[string]any) { delete(m["backup"].(map[string]any), "includeAcl") }},
		{"missing_zero", func(m map[string]any) { delete(m["validation"].(map[string]any), "minTableDataEntries") }},
		{"wrong_case", func(m map[string]any) {
			b := m["backup"].(map[string]any)
			b["IncludeAcl"] = b["includeAcl"]
			delete(b, "includeAcl")
		}},
		{"case_collision", func(m map[string]any) { m["backup"].(map[string]any)["IncludeAcl"] = false }},
		{"wrong_bool", func(m map[string]any) { m["backup"].(map[string]any)["includeAcl"] = 0 }},
		{"ratio", func(m map[string]any) {
			m["observability"].(map[string]any)["sizeAnomaly"].(map[string]any)["minRatio"] = 3
		}},
		{"samples", func(m map[string]any) {
			m["observability"].(map[string]any)["sizeAnomaly"].(map[string]any)["minSamples"] = 99
		}},
		{"unsafe_delete", func(m map[string]any) { m["retention"].(map[string]any)["dryRun"] = false }},
		{"http", func(m map[string]any) { m["s3"].(map[string]any)["endpoint"] = "http://example.com" }},
		{"userinfo", func(m map[string]any) { m["s3"].(map[string]any)["endpoint"] = "https://user:secret@example.com" }},
		{"dot_prefix", func(m map[string]any) { m["s3"].(map[string]any)["prefix"] = "a/../b/" }},
		{"major", func(m map[string]any) { m["postgresql"].(map[string]any)["majorVersion"] = 18 }},
		{"legacy_scope", func(m map[string]any) { m["retention"].(map[string]any)["legacy"].(map[string]any)["enabled"] = true }},
		{"legacy_regex", func(m map[string]any) {
			l := m["retention"].(map[string]any)["legacy"].(map[string]any)
			l["enabled"] = true
			l["prefix"] = "a/"
			l["filenamePattern"] = "^[a$"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var m map[string]any
			_ = json.Unmarshal(data, &m)
			tc.change(m)
			b, _ := json.Marshal(m)
			if _, err := ParseConfig(b); err == nil {
				t.Fatal("accepted invalid config")
			}
		})
	}

	duplicate := strings.Replace(string(data), `"includeAcl":true`, `"includeAcl":true,"includeAcl":false`, 1)
	if _, err := ParseConfig([]byte(duplicate)); err == nil {
		t.Fatal("Viper accepted duplicate configuration keys")
	}

	for _, raw := range []string{`{"a":1,"a":2}`, `{"nested":{"x":1,"x":2}}`, `[`, `{} {}`, `{"a":[{"b":1,"b":2}]}`} {
		if uniqueJSON(json.NewDecoder(strings.NewReader(raw))) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}

	dir := t.TempDir()
	if _, err := LoadConfig(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing config")
	}

	if _, err := LoadCredentials(dir); err == nil {
		t.Fatal("missing creds")
	}

	for _, key := range []string{"access_key", "secret_key"} {
		_ = os.WriteFile(filepath.Join(dir, key), []byte("fixture-secret\n"), 0600)
	}

	got, err := LoadCredentials(dir)
	if err != nil || got.AccessKey != "fixture-secret" || got.Token != "" {
		t.Fatal(got, err)
	}

	_ = os.WriteFile(filepath.Join(dir, "session_token"), []byte("token"), 0600)
	got, err = LoadCredentials(dir)
	if err != nil || got.Token != "token" {
		t.Fatal(got, err)
	}

	_ = os.WriteFile(filepath.Join(dir, "secret_key"), nil, 0600)
	if _, err = LoadCredentials(dir); err == nil {
		t.Fatal("empty creds")
	}
}

func TestArchive(t *testing.T) {
	c := fixtureConfig(t)
	pgEnvironment(t)
	if _, err := PGEnv(c, true); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PGPASSWORD", "")
	if _, err := PGEnv(c, false); err == nil {
		t.Fatal("missing password")
	}

	c.Backup.IncludeSchemas = []string{"-odd", "space schema", "$(unsafe)"}
	c.Backup.ExcludeSchemas = []string{"release_archive_*"}
	c.Backup.ExcludeTables = []string{"a.b"}
	c.Backup.ExcludeTableData = []string{"a.c"}
	c.Backup.IncludeACL = false
	c.Backup.IncludeLargeObjects = false
	c.Backup.StrictNames = false
	args := strings.Join(DumpArgs(c, "/work/a"), "\n")
	for _, value := range []string{"--schema=-odd", "--schema=$(unsafe)", "--no-large-objects", "--no-acl", "--exclude-table=a.b"} {
		if !strings.Contains(args, value) {
			t.Fatal(args)
		}
	}

	inv := fixtureInventory()
	policy := Validation{[]string{"public", "odd schema"}, []RequiredTable{{"public", "root", true}}, 1}
	if _, err := ValidateTOC(fixtureTOC, inv, inv, policy); err != nil {
		t.Fatal(err)
	}

	for _, failure := range []string{"ddl", "minimum", "schema", "table", "data", "rls", "permissions", "foreign"} {
		t.Run(failure, func(t *testing.T) {
			before, after := fixtureInventory(), fixtureInventory()
			p := policy
			text := fixtureTOC
			switch failure {
			case "ddl":
				after.Tables[1].OID = 102
			case "minimum":
				p.MinTableDataEntries = 9
			case "schema":
				p.RequiredSchemas = []string{"missing"}
			case "table":
				p.RequiredTables = []RequiredTable{{"public", "missing", true}}
			case "data":
				text = strings.ReplaceAll(text, "TABLE DATA", "OTHER")
			case "rls":
				before.Tables[1].RLS = true
				after = before
			case "permissions":
				before.Tables[1].Select = false
				after = before
			case "foreign":
				before.Tables[1].Kind = "f"
				after = before
			}

			if _, err := ValidateTOC(text, before, after, p); err == nil {
				t.Fatal("accepted")
			}
		})
	}
	// TABLE in schema DATA must not masquerade as TABLE DATA.
	if _, err := ValidateTOC("1; 1259 101 TABLE DATA child owner\n", inv, inv, Validation{MinTableDataEntries: 1}); err == nil {
		t.Fatal("descriptor ambiguity")
	}

	path := filepath.Join(t.TempDir(), "file")
	_ = os.WriteFile(path, []byte("data"), 0600)
	n, sha, err := HashFile(path)
	if err != nil || n != 4 || len(sha) != 64 {
		t.Fatal(n, sha, err)
	}

	if _, _, err = HashFile("/missing"); err == nil {
		t.Fatal("missing file")
	}
}

type fakeCommand struct {
	run func(context.Context, []string, []string, io.Writer) (string, error)
}

func (f fakeCommand) Run(c context.Context, a, e []string, w io.Writer) (string, error) {
	return f.run(c, a, e, w)
}

func TestVersionsAndProcesses(t *testing.T) {
	c := fixtureConfig(t)
	inv := fixtureInventory()
	ctx := context.Background()
	for _, version := range []string{"16.5", "17.1", "18.0", "invalid"} {
		p := fakeCommand{func(_ context.Context, a, e []string, w io.Writer) (string, error) {
			return a[0] + " (PostgreSQL) " + version, nil
		}}
		_, err := ReadVersions(ctx, p, c, inv)
		if (err == nil) != (version == "16.5") {
			t.Fatal(version, err)
		}
	}

	inv.ServerVersionNum = 170000
	if _, err := ReadVersions(ctx, fakeCommand{}, c, inv); err == nil {
		t.Fatal("server major")
	}

	p := Processes{time.Second}
	out, err := p.Run(ctx, []string{"sh", "-c", "printf ok"}, nil, nil)
	if err != nil || out != "ok" {
		t.Fatal(out, err)
	}

	if _, err = p.Run(ctx, []string{"sh", "-c", "exit 2"}, nil, nil); err == nil {
		t.Fatal("process failure")
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = p.Run(cancelled, []string{"true"}, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}

	limited, stop := context.WithTimeout(ctx, 50*time.Millisecond)
	defer stop()
	if _, err = p.Run(limited, []string{"sh", "-c", "trap '' TERM; sleep 30"}, nil, nil); !errors.Is(
		err,
		context.DeadlineExceeded,
	) {
		t.Fatal(err)
	}
}

type stored struct {
	raw  []byte
	when time.Time
}

type memoryStore struct {
	objects   map[string]stored
	deleted   []string
	fail      string
	version   string
	published bool
}

func newMemory() *memoryStore { return &memoryStore{objects: map[string]stored{}} }
func (s *memoryStore) put(key string, v any, date time.Time) {
	raw, ok := v.([]byte)
	if !ok {
		raw, _ = json.Marshal(v)
	}

	s.objects[key] = stored{raw, date}
}

func (s *memoryStore) Upload(ctx context.Context, path, key string) error {
	if s.fail == "upload" {
		return errors.New("secret")
	}

	raw, err := os.ReadFile(path)
	s.put(key, raw, time.Now())
	return err
}

func (s *memoryStore) Verify(ctx context.Context, key string, size int64, sha string) error {
	if s.fail == "verify" {
		return errors.New("secret")
	}

	return ctx.Err()
}

func (s *memoryStore) Publish(ctx context.Context, key string, m Manifest) error {
	if s.fail == "publish" {
		return errors.New("secret")
	}

	s.published = true
	s.put(key, m, time.Now())
	return ctx.Err()
}

func (s *memoryStore) ReadManifest(ctx context.Context, key string) (Manifest, error) {
	var m Manifest
	if s.fail == "read" {
		return m, errors.New("secret")
	}

	o, ok := s.objects[key]
	if !ok {
		return m, errors.New("absent")
	}

	err := decode(o.raw, manifestSchema, &m)
	return m, err
}

func (s *memoryStore) Head(ctx context.Context, key string) (int64, error) {
	if s.fail == "head" {
		return 0, errors.New("head")
	}

	o, ok := s.objects[key]
	if !ok {
		return 0, errors.New("absent")
	}

	return int64(len(o.raw)), nil
}

func (s *memoryStore) Absent(ctx context.Context, key string) (bool, error) {
	if s.fail == "absent" {
		return false, errors.New("uncertain")
	}

	_, ok := s.objects[key]
	return !ok, nil
}

func (s *memoryStore) List(ctx context.Context, prefix string) ([]Object, error) {
	if s.fail == "list" {
		return nil, errors.New("incomplete")
	}

	objects := []Object{}
	for k, o := range s.objects {
		if strings.HasPrefix(k, prefix) {
			objects = append(objects, Object{k, o.when, int64(len(o.raw))})
		}
	}

	return objects, nil
}

func (s *memoryStore) Delete(ctx context.Context, key string) error {
	if s.fail == "delete" {
		return errors.New("denied")
	}

	s.deleted = append(s.deleted, key)
	delete(s.objects, key)
	return nil
}

func (s *memoryStore) Versioning(context.Context) (string, error) {
	if s.fail == "version" {
		return "", errors.New("denied")
	}

	return s.version, nil
}

func addRun(t *testing.T, s *memoryStore, c Config, n int, date time.Time) Manifest {
	t.Helper()
	id := fmt.Sprintf("20261020T233000Z-%032x", n)
	m := Manifest{
		FormatVersion:            2,
		RunID:                    id,
		Database:                 "fixture",
		ArchiveKey:               c.S3.Prefix + id + "/backup.dump",
		ArchiveBytes:             4,
		ArchiveSHA256:            strings.Repeat("a", 64),
		DumpStartedAt:            "2026-10-20T00:00:00Z",
		DumpFinishedAt:           "2026-10-20T01:00:00Z",
		VerificationFinishedAt:   "2026-10-20T02:00:00Z",
		ImageVersion:             "0.2.0",
		DumpMajor:                16,
		RequiredRestoreMajor:     16,
		TargetServerMajor:        16,
		ConfigurationFingerprint: strings.Repeat("f", 64),
		Filters:                  c.Backup,
		Checks:                   Checks{TOCResult{1, true}, true, true},
		VerificationScope:        VerificationScope,
		Status:                   "completed",
		Versions: Versions{
			"16.3",
			"pg_dump (PostgreSQL) 16.15",
			"pg_restore (PostgreSQL) 16.15",
		},
	}

	s.put(m.ArchiveKey, []byte("data"), date)
	s.put(c.S3.Prefix+id+"/manifest.json", m, date)
	return m
}

func TestRetention(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for _, age := range []time.Duration{364 * 24 * time.Hour, 365 * 24 * time.Hour, 365*24*time.Hour + time.Second} {
		t.Run(age.String(), func(t *testing.T) {
			c := fixtureConfig(t)
			c.Retention.Enabled = true
			c.Retention.DryRun = false
			c.Retention.DeletionApproved = true
			c.Retention.KeepLast = 1
			s := newMemory()
			current := addRun(t, s, c, 100, now)
			old := addRun(t, s, c, 1, now.Add(-age))
			_, err := Rotate(ctx, s, c, current.RunID, now)
			if err != nil {
				t.Fatal(err)
			}

			_, exists := s.objects[old.ArchiveKey]
			if exists != (age <= 365*24*time.Hour) {
				t.Fatal("boundary", age)
			}
		})
	}

	for _, dry := range []bool{true, false} {
		c := fixtureConfig(t)
		c.Retention.Enabled = true
		c.Retention.DryRun = dry
		c.Retention.DeletionApproved = true
		s := newMemory()
		var current Manifest
		for n := 0; n < 9; n++ {
			m := addRun(t, s, c, n, now.Add(-time.Duration(400+n)*24*time.Hour))
			if n == 0 {
				current = m
			}
		}

		plan, err := Rotate(ctx, s, c, current.RunID, now)
		if err != nil || len(plan) != 4 {
			t.Fatal(plan, err)
		}

		if !dry && (len(s.deleted) != 4 || !strings.HasSuffix(s.deleted[0], "manifest.json")) {
			t.Fatal(s.deleted)
		}

		if dry && len(s.deleted) > 0 {
			t.Fatal("dry deletes")
		}
	}

	for _, failure := range []string{"future", "corrupt", "scope", "missing_archive", "size", "read", "head", "list", "version", "absent", "unknown_file", "no_current", "versioning"} {
		t.Run(failure, func(t *testing.T) {
			c := fixtureConfig(t)
			c.Retention.Enabled = true
			c.Retention.DryRun = false
			c.Retention.DeletionApproved = true
			s := newMemory()
			current := addRun(t, s, c, 0, now)
			old := addRun(t, s, c, 1, now.Add(-800*24*time.Hour))
			key := c.S3.Prefix + old.RunID + "/manifest.json"
			switch failure {
			case "future":
				old.FormatVersion = 3
				s.put(key, old, now)
			case "corrupt":
				s.put(key, []byte("{broken"), now)
			case "scope":
				old.VerificationScope = "other"
				s.put(key, old, now)
			case "missing_archive":
				delete(s.objects, old.ArchiveKey)
			case "size":
				old.ArchiveBytes = 99
				s.put(key, old, now)
			case "unknown_file":
				s.put(c.S3.Prefix+"unknown/file", []byte("x"), now)
			case "no_current":
				current.RunID = "absent"
			case "versioning":
				s.version = "Suspended"
			case "absent":
				delete(s.objects, key)
				s.fail = failure
			default:
				s.fail = failure
			}

			if _, err := Rotate(ctx, s, c, current.RunID, now); err == nil {
				t.Fatal("accepted ambiguous")
			}

			if len(s.deleted) > 0 {
				t.Fatal("deleted on error")
			}
		})
	}

	for _, legacyDry := range []bool{true, false} {
		c := fixtureConfig(t)
		c.Retention.Enabled = true
		c.Retention.DryRun = false
		c.Retention.DeletionApproved = true
		c.Retention.Legacy = Legacy{true, legacyDry, "legacy/", `^old\.sql\.gz$`, 7}
		s := newMemory()
		current := addRun(t, s, c, 0, now)
		for n := 1; n < 7; n++ {
			addRun(t, s, c, n, now)
		}

		s.put("legacy/old.sql.gz", []byte("old"), now.Add(-366*24*time.Hour))
		s.put("legacy/nested/old.sql.gz", []byte("old"), now.Add(-999*24*time.Hour))
		id := "20261020T233000Z-" + strings.Repeat("a", 32)
		orphan := c.S3.Prefix + id + "/backup.dump"
		s.put(orphan, []byte("partial"), now.Add(-8*24*time.Hour))
		_, err := Rotate(ctx, s, c, current.RunID, now)
		if err != nil {
			t.Fatal(err)
		}

		_, exists := s.objects["legacy/old.sql.gz"]
		if exists != legacyDry {
			t.Fatal("legacy dry")
		}

		if _, exists = s.objects[orphan]; exists {
			t.Fatal("old orphan retained")
		}
	}

	c := fixtureConfig(t)
	s := newMemory()
	if _, err := Rotate(ctx, s, c, "none", now); err != nil {
		t.Fatal(err)
	}

	runs := []Run{}
	m := addRun(t, s, c, 1, now)
	for n := 2; n < 6; n++ {
		x := addRun(t, s, c, n, now)
		runs = append(runs, Run{ID: x.RunID, Manifest: x})
	}

	for _, size := range []int64{1, 4, 40} {
		m.ArchiveBytes = size
		got := SizeAnomaly(runs, m, c.Observability.SizeAnomaly)
		if got != (size != 4) {
			t.Fatal(size, got)
		}
	}

	if SizeAnomaly(nil, m, c.Observability.SizeAnomaly) {
		t.Fatal("no samples")
	}
}

func TestRunner(t *testing.T) {
	for _, stage := range []string{"success", "preflight", "dump", "local_validation", "upload", "verify", "publish", "retention", "cancel", "empty_archive", "catalog_invalid", "version_invalid"} {
		t.Run(stage, func(t *testing.T) {
			c := fixtureConfig(t)
			pgEnvironment(t)
			s := newMemory()
			if stage == "upload" || stage == "verify" || stage == "publish" {
				s.fail = stage
			}

			if stage == "retention" {
				s.fail = "list"
			}

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			inv := fixtureInventory()
			raw, _ := json.Marshal(inv)
			p := fakeCommand{func(ctx context.Context, a, e []string, w io.Writer) (string, error) {
				if stage == "preflight" {
					return "", errors.New("secret")
				}

				if a[0] == "psql" {
					if stage == "catalog_invalid" {
						return "{invalid", nil
					}

					return string(raw), nil
				}

				if a[1] == "--version" {
					if stage == "version_invalid" {
						return "pg_dump (PostgreSQL) 18.0", nil
					}

					return a[0] + " (PostgreSQL) 16.15", nil
				}

				if a[0] == "pg_dump" {
					if stage == "dump" {
						return "", errors.New("disk full")
					}

					for _, v := range a {
						if strings.HasPrefix(v, "--file=") {
							contents := []byte("archive")
							if stage == "empty_archive" {
								contents = nil
							}

							_ = os.WriteFile(strings.TrimPrefix(v, "--file="), contents, 0600)
						}
					}

					return "", nil
				}

				if a[0] == "pg_restore" {
					if stage == "local_validation" {
						return "", errors.New("corrupt")
					}

					if w != nil {
						_, _ = io.WriteString(w, fixtureTOC)
					}

					if stage == "cancel" {
						cancel()
						return "", ctx.Err()
					}

					return "", nil
				}

				return "", errors.New("unexpected")
			}}
			var logs bytes.Buffer
			r := Runner{c, p, s, "0.2.0", &logs, time.Now}
			code := r.Run(ctx, t.TempDir(), "20261020T233000Z-"+strings.Repeat("b", 32))
			good := stage == "success" || stage == "retention"
			if (code == 0) != good {
				t.Fatal(code, logs.String())
			}

			if !good && s.published {
				t.Fatal("premature manifest")
			}

			if strings.Contains(logs.String(), "secret") {
				t.Fatal("secret leak")
			}

			if stage == "retention" && !strings.Contains(logs.String(), "backup_retention_failed") {
				t.Fatal(logs.String())
			}
		})
	}
}

func TestRetentionReserveAndFailures(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	for _, n := range []int{1, 6, 7} {
		c := fixtureConfig(t)
		c.Retention.Enabled = true
		c.Retention.DryRun = false
		c.Retention.DeletionApproved = true
		s := newMemory()
		var current Manifest
		for i := 0; i < n; i++ {
			current = addRun(t, s, c, i, now.Add(-999*24*time.Hour))
		}

		if _, err := Rotate(ctx, s, c, current.RunID, now); err != nil || len(s.deleted) != 0 {
			t.Fatal(n, s.deleted, err)
		}
	}

	for _, failure := range []string{"delete", "unapproved", "protected", "orphan_fresh"} {
		t.Run(failure, func(t *testing.T) {
			c := fixtureConfig(t)
			c.Retention.Enabled = true
			c.Retention.DryRun = false
			c.Retention.DeletionApproved = true
			s := newMemory()
			current := addRun(t, s, c, 0, now)
			for n := 1; n < 10; n++ {
				addRun(t, s, c, n, now.Add(-time.Duration(400+n)*24*time.Hour))
			}

			switch failure {
			case "delete":
				s.fail = "delete"
			case "unapproved":
				c.Retention.DeletionApproved = false
			case "protected":
				s.objects[current.ArchiveKey] = stored{[]byte("bad-size"), now}
			case "orphan_fresh":
				key := c.S3.Prefix + "20261020T233000Z-" + strings.Repeat("e", 32) + "/backup.dump"
				s.put(key, []byte("fresh"), now.Add(-time.Hour))
			}

			_, err := Rotate(ctx, s, c, current.RunID, now)
			if failure != "orphan_fresh" && err == nil {
				t.Fatal("expected refusal")
			}

			if failure == "orphan_fresh" && err != nil {
				t.Fatal(err)
			}
		})
	}
	// An even-sized history uses the median of both middle values.
	c := fixtureConfig(t)
	m := addRun(t, newMemory(), c, 99, now)
	runs := []Run{}
	for n := 0; n < 4; n++ {
		x := m
		x.RunID = fmt.Sprint(n)
		x.ArchiveBytes = int64(n + 1)
		runs = append(runs, Run{ID: x.RunID, Manifest: x})
	}

	if SizeAnomaly(runs, m, c.Observability.SizeAnomaly) {
		t.Fatal("median")
	}

	id, err := NewRunID(now)
	if err != nil || !runPattern.MatchString(id) {
		t.Fatal(id, err)
	}
}

func TestAdditionalValidation(t *testing.T) {
	c := fixtureConfig(t)
	m := addRun(t, newMemory(), c, 1, time.Now())
	if ValidateManifest(m, c.S3.Prefix, "different") == nil {
		t.Fatal("manifest identity")
	}

	inv := fixtureInventory()
	if _, err := ValidateTOC(fixtureTOC, inv, inv, Validation{RequiredTables: []RequiredTable{{"public", "root", false}}}); err != nil {
		t.Fatal(err)
	}

	for _, text := range []string{"bad-json", ""} {
		p := fakeCommand{
			func(context.Context, []string, []string, io.Writer) (string, error) { return text, nil },
		}

		if _, err := ReadInventory(context.Background(), p, nil); err == nil {
			t.Fatal("catalog malformed")
		}
	}

	failed := fakeCommand{
		func(context.Context, []string, []string, io.Writer) (string, error) { return "", io.ErrUnexpectedEOF },
	}

	if _, err := ReadVersions(context.Background(), failed, c, inv); err == nil {
		t.Fatal("tool failure")
	}

	if err := decode([]byte("{}"), []byte("invalid"), &Config{}); err == nil {
		t.Fatal("invalid schema")
	}

	if err := writeReport("/missing/parent/file", map[string]int{"a": 1}); err == nil {
		t.Fatal("report disk error")
	}

	if err := writeReport(filepath.Join(t.TempDir(), "file"), make(chan int)); err == nil {
		t.Fatal("unencodable")
	}
}

func TestRetentionDurationBounds(t *testing.T) {
	c := fixtureConfig(t)
	raw, _ := json.Marshal(c)
	for _, field := range []string{"maxAgeDays", "incompleteRunMaxAgeDays"} {
		for _, days := range []int{36500, 36501, 106752, 2147483647} {
			var value map[string]any
			_ = json.Unmarshal(raw, &value)
			value["retention"].(map[string]any)[field] = days
			data, _ := json.Marshal(value)
			_, err := ParseConfig(data)
			if (err == nil) != (days == 36500) {
				t.Fatal(field, days, err)
			}
		}
	}
}

func TestRotateRejectsOverflowBeforeAnyDelete(t *testing.T) {
	c := fixtureConfig(t)
	c.Retention.Enabled = true
	c.Retention.DryRun = false
	c.Retention.DeletionApproved = true
	for _, days := range []int{0, 106752, 2147483647} {
		c.Retention.MaxAgeDays = days
		s := newMemory()
		if _, err := Rotate(context.Background(), s, c, "unused", time.Now()); err == nil {
			t.Fatal("overflow accepted", days)
		}

		if len(s.deleted) > 0 {
			t.Fatal("deleted")
		}
	}
}

func TestLegacyCannotDeleteProtectedManagedObjects(t *testing.T) {
	c := fixtureConfig(t)
	c.Retention.Enabled = true
	c.Retention.DryRun = false
	c.Retention.DeletionApproved = true
	now := time.Now().UTC()
	s := newMemory()
	m := addRun(t, s, c, 1, now.Add(-999*24*time.Hour))
	c.Retention.Legacy = Legacy{true, false, c.S3.Prefix + m.RunID + "/", `^.*$`, 1}
	if _, err := Rotate(context.Background(), s, c, m.RunID, now); err != nil {
		t.Fatal(err)
	}

	if len(s.deleted) > 0 {
		t.Fatal("legacy bypassed keepLast", s.deleted)
	}
}
