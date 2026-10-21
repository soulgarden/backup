package backup

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestCharts(t *testing.T) {
	helm := os.Getenv("HELM")
	if helm == "" {
		helm = "helm"
	}

	if _, err := exec.LookPath(helm); err != nil {
		t.Skip("Helm is checked in CI")
	}

	charts := []string{"../../helm/backup"}
	if extra := os.Getenv("BACKUP_CI_CHART"); extra != "" {
		charts = append(charts, extra)
	}

	for _, chart := range charts {
		t.Run(filepath.Base(filepath.Dir(chart)), func(t *testing.T) {
			version, err := os.ReadFile("../../VERSION")
			if err != nil {
				t.Fatal(err)
			}

			var metadata struct {
				AppVersion string `yaml:"appVersion"`
			}

			raw, err := os.ReadFile(filepath.Join(chart, "Chart.yaml"))
			if err != nil {
				t.Fatal(err)
			}

			if yaml.Unmarshal(raw, &metadata) != nil {
				t.Fatal("metadata")
			}

			var values struct {
				Image struct {
					Tag string `yaml:"tag"`
				} `yaml:"image"`
			}

			raw, err = os.ReadFile(filepath.Join(chart, "values.yaml"))
			if err != nil {
				t.Fatal(err)
			}

			if yaml.Unmarshal(raw, &values) != nil {
				t.Fatal("values")
			}

			if metadata.AppVersion != strings.TrimSpace(string(version)) ||
				values.Image.Tag != metadata.AppVersion {
				t.Fatal("version drift")
			}

			if out, err := exec.Command(helm, "lint", "--strict", chart).CombinedOutput(); err != nil {
				t.Fatal(string(out), err)
			}

			render := func(args ...string) string {
				t.Helper()
				cmd := exec.Command(helm, append([]string{"template", "backup", chart}, args...)...)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatal(string(out), err)
				}

				return string(out)
			}

			docs, err := readChartDocuments(render())
			if err != nil {
				t.Fatal(err)
			}

			for _, doc := range docs {
				if doc["kind"] == "ConfigMap" {
					data := doc["data"].(map[string]any)
					if _, err := ParseConfig([]byte(data["config.json"].(string))); err != nil {
						t.Fatal(err)
					}

				}

				if doc["kind"] == "CronJob" {
					raw, _ := json.Marshal(doc)
					for _, expected := range []string{`"activeDeadlineSeconds":10800`, `"terminationGracePeriodSeconds":90`, `"runAsUser":10001`, `"automountServiceAccountToken":false`} {
						if !strings.Contains(string(raw), expected) {
							t.Fatal(expected)
						}
					}
				}
			}

			digest := "sha256:" + strings.Repeat("a", 64)
			if !strings.Contains(render("--set", "image.digest="+digest), "@"+digest) {
				t.Fatal("digest")
			}

			for _, arg := range []string{"s3.host_bucket=old", "backup.verifyRemoteSha256=false", "retention.keepLast=0", "suspend=false"} {
				cmd := exec.Command(helm, "template", "backup", chart, "--set", arg)
				if cmd.Run() == nil {
					t.Fatal("accepted", arg)
				}
			}
		})
	}
}

func readChartDocuments(rendered string) ([]map[string]any, error) {
	decoder := yaml.NewDecoder(strings.NewReader(rendered))
	var docs []map[string]any
	counts := map[string]int{}

	for {
		var doc map[string]any
		err := decoder.Decode(&doc)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, err
		}
		if len(doc) == 0 {
			continue
		}

		kind, _ := doc["kind"].(string)
		counts[kind]++
		docs = append(docs, doc)
	}

	if counts["ConfigMap"] != 1 || counts["CronJob"] != 1 {
		return nil, errors.New("expected exactly one ConfigMap and one CronJob")
	}
	return docs, nil
}

func TestChartDocuments(t *testing.T) {
	config := "kind: ConfigMap\n"
	cron := "kind: CronJob\n"
	for _, tc := range []struct {
		name string
		yaml string
		ok   bool
	}{
		{"valid", config + "---\n" + cron, true},
		{"empty documents", "---\n---\n" + config + "---\n" + cron, true},
		{"missing cronjob", config, false},
		{"missing config", cron, false},
		{"duplicate cronjob", config + "---\n" + cron + "---\n" + cron, false},
		{"duplicate config", config + "---\n" + config + "---\n" + cron, false},
		{"malformed cronjob", config + "---\n" + cron + "spec: [", false},
		{"duplicate yaml key", config + "---\n" + cron + cron, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readChartDocuments(tc.yaml)
			if (err == nil) != tc.ok {
				t.Fatalf("accepted=%t, want %t: %v", err == nil, tc.ok, err)
			}
		})
	}
}
