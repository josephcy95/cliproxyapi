package openai

import (
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
)

func TestAntigravityModelListsExposeFamiliesAndEfforts(t *testing.T) {
	reg := registry.GetGlobalRegistry()
	var variants []*registry.ModelInfo
	for _, family := range []string{"gemini-4-argon", "gemini-4-hydrogen", "gemini-3.8-flash", "claude-opus-5-5"} {
		levels := []string{"low", "medium", "high"}
		if family == "gemini-4-hydrogen" {
			levels = append(levels, "xhigh")
		}
		for _, level := range levels {
			variants = append(variants, &registry.ModelInfo{ID: family + "-" + level, Type: "antigravity", OwnedBy: "antigravity", Thinking: &registry.ThinkingSupport{Levels: levels}})
		}
	}
	reg.RegisterClient(t.Name(), "antigravity", registry.GroupAntigravityEffortModels(variants))
	t.Cleanup(func() { reg.UnregisterClient(t.Name()) })
	h := NewOpenAIAPIHandler(handlers.NewBaseAPIHandlers(&config.SDKConfig{}, nil))
	for _, query := range []string{"", "?client_version=0.137.0"} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest("GET", "/v1/models"+query, nil)
		h.OpenAIModels(c)
		if w.Code != 200 {
			t.Fatalf("%s: status=%d: %s", query, w.Code, w.Body.String())
		}
		var response struct {
			Data   []map[string]any `json:"data"`
			Models []map[string]any `json:"models"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		models := response.Data
		key := "id"
		if query != "" {
			models = response.Models
			key = "slug"
		}
		found := 0
		for _, m := range models {
			id, _ := m[key].(string)
			if !strings.HasPrefix(id, "gemini-4-") && id != "gemini-3.8-flash" && !strings.HasPrefix(id, "claude-opus-5-5") {
				continue
			}
			base, effort := registry.AntigravityEffortFamily(id)
			if effort != "" {
				t.Fatalf("%s: legacy ID advertised: %s", query, id)
			}
			found++
			var got []string
			levels, _ := m["supported_reasoning_levels"].([]any)
			for _, v := range levels {
				switch v := v.(type) {
				case string:
					got = append(got, v)
				case map[string]any:
					got = append(got, v["effort"].(string))
				}
			}
			want := []string{"low", "medium", "high"}
			if base == "gemini-4-hydrogen" {
				want = append(want, "xhigh")
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s/%s: efforts=%v, want %v", query, id, got, want)
			}
		}
		if found != 4 {
			t.Fatalf("%s: found %d families, want 4: %s", query, found, w.Body.String())
		}
	}
}
