package backup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublicationDecision(t *testing.T) {
	script, err := filepath.Abs("../../scripts/check-unpublished.sh")
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name    string
		status  string
		version string
		mode    string
		curlErr string
		want    string
		ok      bool
	}{
		{"new version", "404", "0.2.0", "--if-needed", "0", "publish=true\n", true},
		{"existing version", "200", "0.2.0", "--if-needed", "0", "publish=false\n", true},
		{"strict absent", "404", "0.2.0", "", "0", "", true},
		{"strict existing", "200", "0.2.0", "", "0", "", false},
		{"authentication failed", "401", "0.2.0", "--if-needed", "0", "", false},
		{"rate limited", "429", "0.2.0", "--if-needed", "0", "", false},
		{"registry failure", "500", "0.2.0", "--if-needed", "0", "", false},
		{"redirect", "301", "0.2.0", "--if-needed", "0", "", false},
		{"network failure", "000", "0.2.0", "--if-needed", "28", "", false},
		{"partial failed response", "404", "0.2.0", "--if-needed", "28", "", false},
		{"invalid version", "404", "latest", "--if-needed", "0", "", false},
		{"invalid mode", "404", "0.2.0", "--unsafe", "0", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "VERSION"), []byte(tc.version), 0600); err != nil {
				t.Fatal(err)
			}
			curl := "#!/bin/sh\nprintf '%s' \"$FIXTURE_STATUS\"\nexit \"$FIXTURE_EXIT\"\n"
			if err := os.WriteFile(filepath.Join(dir, "curl"), []byte(curl), 0700); err != nil {
				t.Fatal(err)
			}

			args := []string{script}
			if tc.mode != "" {
				args = append(args, tc.mode)
			}
			cmd := exec.Command("bash", args...)
			cmd.Dir = dir
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"),
				"FIXTURE_STATUS="+tc.status, "FIXTURE_EXIT="+tc.curlErr)
			var stderr strings.Builder
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if (err == nil) != tc.ok || string(out) != tc.want {
				t.Fatalf("output=%q err=%v stderr=%s", out, err, stderr.String())
			}
		})
	}
}
