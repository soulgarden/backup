package backup

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed inventory.sql
var inventorySQL string

type Command interface {
	Run(context.Context, []string, []string, io.Writer) (string, error)
}

type Processes struct {
	Grace time.Duration
}

func (p Processes) Run(ctx context.Context, args, env []string, out io.Writer) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = env
	if env == nil {
		cmd.Env = os.Environ()
	}

	cmd.Stderr = io.Discard
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = p.Grace / 2
	var b bytes.Buffer
	if out != nil {
		cmd.Stdout = out
	} else {
		cmd.Stdout = &b
	}

	err := cmd.Run()
	// The group can outlive its leader; kill any descendants after cancellation.
	if ctx.Err() != nil {
		if cmd.Process != nil {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}

		return "", ctx.Err()
	}

	if err != nil {
		return "", errors.New("PROCESS_FAILED")
	}

	return b.String(), nil
}

type Schema struct {
	OID  uint32 `json:"oid"`
	Name string `json:"name"`
}

type Table struct {
	OID     uint32   `json:"oid"`
	Schema  string   `json:"schema"`
	Name    string   `json:"table"`
	Kind    string   `json:"kind"`
	RLS     bool     `json:"rls"`
	Select  bool     `json:"select"`
	Parents []uint32 `json:"parents"`
}

type Inventory struct {
	ServerVersion    string   `json:"server_version"`
	ServerVersionNum int      `json:"server_version_num"`
	Schemas          []Schema `json:"schemas"`
	Tables           []Table  `json:"tables"`
}

func PGEnv(c Config, metadata bool) ([]string, error) {
	for _, key := range []string{"PGHOST", "PGUSER", "PGDATABASE", "PGPASSWORD"} {
		if os.Getenv(key) == "" {
			return nil, errors.New("MISSING_PG_ENV")
		}
	}

	env := []string{}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "PGOPTIONS=") && !strings.HasPrefix(v, "PGCONNECT_TIMEOUT=") {
			env = append(env, v)
		}
	}

	options := "-c default_transaction_read_only=on"
	if metadata {
		options += " -c statement_timeout=5000"
	}

	return append(
		env,
		"PGOPTIONS="+options,
		"PGCONNECT_TIMEOUT="+strconv.Itoa(c.PostgreSQL.ConnectTimeoutSeconds),
	), nil
}

func ReadInventory(ctx context.Context, p Command, env []string) (Inventory, error) {
	var inv Inventory
	raw, err := p.Run(
		ctx,
		[]string{"psql", "-X", "--no-password", "-At", "-v", "ON_ERROR_STOP=1", "-c", inventorySQL},
		env,
		nil,
	)
	if err != nil {
		return inv, err
	}

	if json.Unmarshal([]byte(raw), &inv) != nil {
		return inv, errors.New("INVALID_CATALOG")
	}

	return inv, nil
}

func DumpArgs(c Config, path string) []string {
	b := c.Backup
	a := []string{
		"pg_dump",
		"--no-password",
		"--format=custom",
		"--compress=gzip:" + strconv.Itoa(b.CompressionLevel),
		"--lock-wait-timeout=" + strconv.Itoa(c.PostgreSQL.LockWaitTimeoutSeconds) + "s",
		"--file=" + path,
	}

	for _, f := range []struct {
		flag     string
		patterns []string
	}{{"schema", b.IncludeSchemas}, {"exclude-schema", b.ExcludeSchemas}, {"exclude-table", b.ExcludeTables}, {"exclude-table-data", b.ExcludeTableData}} {
		for _, v := range f.patterns {
			a = append(a, "--"+f.flag+"="+v)
		}
	}

	if b.IncludeLargeObjects {
		a = append(a, "--large-objects")
	} else {
		a = append(a, "--no-large-objects")
	}

	if !b.IncludeACL {
		a = append(a, "--no-acl")
	}

	if b.StrictNames {
		a = append(a, "--strict-names")
	}

	return a
}

type Versions struct {
	Server  string `json:"server_version"`
	Dump    string `json:"pg_dump_version"`
	Restore string `json:"pg_restore_version"`
}

var versionPattern = regexp.MustCompile(`\(PostgreSQL\) (\d+)\.`)

func ReadVersions(ctx context.Context, p Command, c Config, inv Inventory) (Versions, error) {
	v := Versions{Server: inv.ServerVersion}
	if inv.ServerVersionNum/10000 != c.PostgreSQL.MajorVersion {
		return v, errors.New("SERVER_MAJOR_MISMATCH")
	}

	for _, tool := range []string{"pg_dump", "pg_restore"} {
		raw, err := p.Run(ctx, []string{tool, "--version"}, nil, nil)
		if err != nil {
			return v, err
		}

		m := versionPattern.FindStringSubmatch(raw)
		if len(m) != 2 || m[1] != strconv.Itoa(c.PostgreSQL.MajorVersion) {
			return v, errors.New("CLIENT_MAJOR_MISMATCH")
		}

		if tool == "pg_dump" {
			v.Dump = strings.TrimSpace(raw)
		} else {
			v.Restore = strings.TrimSpace(raw)
		}
	}

	return v, nil
}

type TOCResult struct {
	TableDataEntries       int  `json:"table_data_entries"`
	RequiredObjectsChecked bool `json:"required_objects_checked"`
}

var tocPrefix = regexp.MustCompile(`^\d+;\s+(\d+)\s+(\d+)\s+(.*)$`)

func ValidateTOC(text string, before, after Inventory, policy Validation) (TOCResult, error) {
	result := TOCResult{}
	if !reflect.DeepEqual(before, after) {
		return result, errors.New("CATALOG_CHANGED")
	}

	defs, data, schemas := map[uint32]bool{}, map[uint32]bool{}, map[uint32]bool{}
	for _, line := range strings.Split(text, "\n") {
		m := tocPrefix.FindStringSubmatch(line)
		if len(m) != 4 {
			continue
		}

		cat, _ := strconv.ParseUint(m[1], 10, 32)
		oid, _ := strconv.ParseUint(m[2], 10, 32)
		suffix := m[3]
		switch {
		case cat == 1259 && (strings.HasPrefix(suffix, "TABLE ") || strings.HasPrefix(suffix, "FOREIGN TABLE ")):
			defs[uint32(oid)] = true
		case cat == 0 && strings.HasPrefix(suffix, "TABLE DATA "):
			data[uint32(oid)] = true
		case cat == 2615 && strings.HasPrefix(suffix, "SCHEMA "):
			schemas[uint32(oid)] = true
		}
	}

	if len(data) < policy.MinTableDataEntries {
		return result, errors.New("TOO_FEW_TABLE_DATA")
	}

	for _, name := range policy.RequiredSchemas {
		found := false
		for _, s := range before.Schemas {
			if s.Name == name && schemas[s.OID] {
				found = true
			}
		}

		for _, t := range before.Tables {
			if t.Schema == name && defs[t.OID] {
				found = true
			}
		}

		if !found {
			return result, errors.New("MISSING_SCHEMA")
		}
	}

	for _, required := range policy.RequiredTables {
		var table *Table
		for i := range before.Tables {
			t := &before.Tables[i]
			if t.Schema == required.Schema && t.Name == required.Table {
				table = t
				break
			}
		}

		if table == nil || !defs[table.OID] {
			return result, errors.New("MISSING_TABLE")
		}

		if !required.RequireData {
			continue
		}

		selected := map[uint32]bool{table.OID: true}
		for {
			changed := false
			for _, t := range before.Tables {
				for _, parent := range t.Parents {
					if selected[parent] && !selected[t.OID] {
						selected[t.OID] = true
						changed = true
					}
				}
			}

			if !changed {
				break
			}
		}

		for _, t := range before.Tables {
			if !selected[t.OID] {
				continue
			}

			if t.Kind == "f" {
				return result, errors.New("FOREIGN_TABLE_DATA_UNSUPPORTED")
			}

			if t.Kind == "r" && (t.RLS || !t.Select || !data[t.OID]) {
				return result, errors.New("MISSING_TABLE_DATA")
			}
		}
	}

	return TOCResult{len(data), true}, nil
}

func HashFile(path string) (int64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}

	defer f.Close()
	h := sha256.New()
	n, err := io.CopyBuffer(h, f, make([]byte, 8<<20))
	return n, hex.EncodeToString(h.Sum(nil)), err
}
