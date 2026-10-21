package backup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"testing"
	"time"
)

func TestLogalertContract(t *testing.T) {
	base := os.Getenv("BACKUP_LOGALERT_URL")
	if base == "" {
		t.Skip("disposable Elasticsearch contract")
	}

	index := fmt.Sprintf("backup-contract-%d", time.Now().UnixNano())
	call := func(method, path string, body any) map[string]any {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}

		req, err := http.NewRequest(method, base+"/"+path, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}

		req.Header.Set("Content-Type", "application/json")
		response, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if err != nil {
			t.Fatal(err)
		}

		defer response.Body.Close()
		if response.StatusCode >= 300 {
			raw, _ := io.ReadAll(response.Body)
			t.Fatal(response.StatusCode, string(raw))
		}

		var result map[string]any
		if json.NewDecoder(response.Body).Decode(&result) != nil {
			t.Fatal("invalid ES response")
		}

		return result
	}

	call(
		"PUT",
		index,
		map[string]any{
			"mappings": map[string]any{
				"properties": map[string]any{
					"namespace": map[string]any{"type": "keyword"},
					"message":   map[string]any{"type": "text"},
				},
			},
		},
	)
	defer call("DELETE", index, nil)
	for n, event := range []string{"backup_retention_failed", "backup_size_anomaly"} {
		var line bytes.Buffer
		Emit(
			&line,
			event,
			"retention",
			"20261020T233000Z-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
			"CLEANUP_FAILED",
			"ERROR",
		)
		call(
			"PUT",
			fmt.Sprintf("%s/_doc/%d", index, n),
			map[string]string{"namespace": "swup", "message": line.String()},
		)
	}

	call(
		"PUT",
		index+"/_doc/2",
		map[string]string{"namespace": "kino", "message": "ERROR backup_retention_failed"},
	)
	call("PUT", index+"/_doc/3", map[string]string{"namespace": "swup", "message": "INFO normal completion"})
	call("POST", index+"/_refresh", nil)
	const query = "namespace:swup AND (message:error OR message:fatal OR message:panic OR message:critical) AND NOT (message:warn OR message:warning OR message:info OR message:debug)"
	result := call(
		"POST",
		index+"/_search",
		map[string]any{
			"query":            map[string]any{"query_string": map[string]string{"query": query}},
			"size":             100,
			"track_total_hits": true,
		},
	)
	if result["timed_out"] != false || result["_shards"].(map[string]any)["failed"].(float64) != 0 {
		t.Fatal("incomplete query")
	}

	hits := result["hits"].(map[string]any)["hits"].([]any)
	delivered := 0
	mockSender := func(any) { delivered++ }
	for _, hit := range hits {
		mockSender(hit)
	}

	if delivered != 2 {
		t.Fatal("alert delivery", delivered)
	}
}
