package backup

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestCharts(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
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
			if metadata.AppVersion != strings.TrimSpace(string(version)) || values.Image.Tag != metadata.AppVersion {
				t.Fatal("version drift")
			}
			if out, err := exec.Command("helm", "lint", "--strict", chart).CombinedOutput(); err != nil {
				t.Fatal(string(out), err)
			}
			render := func(args ...string) string {
				t.Helper()
				cmd := exec.Command("helm", append([]string{"template", "backup", chart}, args...)...)
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatal(string(out), err)
				}
				return string(out)
			}
			text := render()
			d := yaml.NewDecoder(strings.NewReader(text))
			found := false
			for {
				var doc map[string]any
				if d.Decode(&doc) != nil {
					break
				}
				if doc["kind"] == "ConfigMap" {
					data := doc["data"].(map[string]any)
					if _, err := ParseConfig([]byte(data["config.json"].(string))); err != nil {
						t.Fatal(err)
					}
					found = true
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
			if !found {
				t.Fatal("config absent")
			}
			digest := "sha256:" + strings.Repeat("a", 64)
			if !strings.Contains(render("--set", "image.digest="+digest), "@"+digest) {
				t.Fatal("digest")
			}
			for _, arg := range []string{"s3.host_bucket=old", "backup.verifyRemoteSha256=false", "retention.keepLast=0", "suspend=false"} {
				cmd := exec.Command("helm", "template", "backup", chart, "--set", arg)
				if cmd.Run() == nil {
					t.Fatal("accepted", arg)
				}
			}
		})
	}
}
