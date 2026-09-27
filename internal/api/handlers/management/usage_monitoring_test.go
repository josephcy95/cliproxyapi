package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore"
)

func TestUsageMonitoringQueries(t *testing.T) {
	store, err := usagestore.Open(usagestore.Options{Path: filepath.Join(t.TempDir(), "usage.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UnixMilli()
	if err := store.Insert(context.Background(), usagestore.Event{TimestampMS: now, Model: "model", Source: "source", Provider: "codex"}); err != nil {
		t.Fatal(err)
	}
	h := &Handler{usageStore: store}
	for _, tc := range []struct {
		name, body string
		handler    func(*gin.Context)
		status     int
	}{
		{"recent", `{"accounts":[{"source":"source","provider":"codex"}]}`, h.GetUsageAccountRecentRequests, 200},
		{"empty", `{"accounts":[]}`, h.GetUsageAccountRecentRequests, 200},
		{"invalid json", `{`, h.GetUsageAccountRecentRequests, 400},
		{"reversed", `{"from_ms":10,"to_ms":1}`, h.GetUsageAccountRecentRequests, 400},
		{"negative", `{"from_ms":-1}`, h.GetUsageAccountRecentRequests, 400},
		{"too many", `{"accounts":[` + strings.Repeat(`{},`, 200) + `{}]}`, h.GetUsageAccountRecentRequests, 400},
		{"facets", `{"fields":["models","providers"]}`, h.GetUsageFilterOptions, 200},
		{"legacy facets", `{}`, h.GetUsageFilterOptions, 200},
		{"unknown facet", `{"fields":["SQL injection"]}`, h.GetUsageFilterOptions, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			tc.handler(c)
			if rec.Code != tc.status {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
			if tc.name == "recent" {
				var response struct {
					Accounts []map[string]json.RawMessage `json:"accounts"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
					t.Fatal(err)
				}
				if len(response.Accounts) != 1 || response.Accounts[0]["recent_requests"] == nil || response.Accounts[0]["total_calls"] != nil || response.Accounts[0]["estimated_cost"] != nil {
					t.Fatalf("not a lightweight response: %s", rec.Body.String())
				}
			}
		})
	}
}

func TestModelPriceRulesHTTPValidation(t *testing.T) {
	store, err := usagestore.Open(usagestore.Options{Path: filepath.Join(t.TempDir(), "usage.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h := &Handler{usageStore: store}
	for _, tc := range []struct {
		name, body string
		status     int
	}{
		{"valid", `{"prices":[{"model":"model","prompt_per_1m":2,"context_tiers":[{"threshold_tokens":100,"prompt_per_1m":0,"prompt_configured":true}]}]}`, 200},
		{"negative tier", `{"replace":true,"prices":[{"model":"model","context_tiers":[{"threshold_tokens":100,"prompt_per_1m":-1,"prompt_configured":true}]}]}`, 400},
		{"duplicate threshold", `{"replace":true,"prices":[{"model":"model","context_tiers":[{"threshold_tokens":100,"prompt_configured":true},{"threshold_tokens":100,"prompt_configured":true}]}]}`, 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPut, "/", strings.NewReader(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")
			h.PutModelPrices(c)
			if rec.Code != tc.status {
				t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
			}
		})
	}
	prices, err := store.LoadModelPrices(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p, ok := prices["model"]
	if !ok || p.PromptPer1M != 2 || len(p.ContextTiers) != 1 || !p.ContextTiers[0].PromptConfigured || p.ContextTiers[0].PromptPer1M != 0 {
		t.Fatalf("rules lost or invalid replacement destroyed prices: %+v", prices)
	}
}
