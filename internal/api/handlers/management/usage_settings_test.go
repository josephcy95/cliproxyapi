package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore"
)

func TestUsageRetentionSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("# keep this comment\nusage-statistics-enabled: true\n"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := usagestore.Open(usagestore.Options{Path: filepath.Join(dir, "usage.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h := &Handler{cfg: &config.Config{}, configFilePath: path, usageStore: store}
	call := func(method, body string) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(method, "/usage-retention-days", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		if method == http.MethodGet {
			h.GetUsageRetentionDays(c)
		} else {
			h.PutUsageRetentionDays(c)
		}
		return rec
	}
	rec := call(http.MethodGet, "")
	var response struct {
		Value   int  `json:"value"`
		Default int  `json:"default_days"`
		Restart bool `json:"restart_required"`
	}
	if err = json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Value != 90 || response.Default != 90 || response.Restart {
		t.Fatalf("unexpected defaults: %s", rec.Body.String())
	}
	now := time.Now()
	for _, days := range []int{10, 45, 100} {
		if err := store.Insert(context.Background(), usagestore.Event{TimestampMS: now.AddDate(0, 0, -days).UnixMilli(), Model: "test"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, body := range []string{`{}`, `{"value":null}`, `{"value":0}`, `{"value":-1}`, `{"value":1.5}`, `{"value":36501}`, `{"value":"30"}`} {
		if r := call(http.MethodPut, body); r.Code != 400 {
			t.Fatalf("%s: %d %s", body, r.Code, r.Body.String())
		}
	}
	if store.RetentionDays() != 90 {
		t.Fatal("invalid request changed live retention")
	}
	if r := call(http.MethodPut, `{"value":30}`); r.Code != 200 {
		t.Fatalf("save: %s", r.Body.String())
	}
	if store.RetentionDays() != 30 {
		t.Fatal("retention was not hot-applied")
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(saved), "usage-retention-days: 30") || !strings.Contains(string(saved), "# keep this comment") {
		t.Fatalf("not persisted: %s", saved)
	}
	// Failed persistence must not shorten the active policy or mutate config.
	h.configFilePath = dir
	if r := call(http.MethodPut, `{"value":1}`); r.Code != 500 {
		t.Fatalf("save failure: %d", r.Code)
	}
	if store.RetentionDays() != 30 || h.cfg.UsageRetentionDays != 30 {
		t.Fatal("failed save changed retention")
	}
	if err := store.PurgeExpired(context.Background()); err != nil {
		t.Fatal(err)
	}
	summary, err := store.GetSummary(context.Background(), usagestore.QueryFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if summary.TotalCalls != 1 {
		t.Fatalf("cleanup should retain 10-day row only: %+v", summary)
	}
}
