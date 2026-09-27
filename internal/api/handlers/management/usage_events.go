package management

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/usagestore"
)

// SetUsageStore attaches the durable usage store used by monitoring endpoints.
func (h *Handler) SetUsageStore(store *usagestore.Store) {
	if h == nil {
		return
	}
	h.mu.Lock()
	h.usageStore = store
	h.mu.Unlock()
}

// UsageStore returns the current usage store (may be nil).
func (h *Handler) UsageStore() *usagestore.Store {
	if h == nil {
		return nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.usageStore
}

func (h *Handler) requireUsageStore(c *gin.Context) *usagestore.Store {
	store := h.UsageStore()
	if store == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{
			"error": "usage store unavailable; enable usage-statistics-enabled and ensure usage-store-path is writable",
		})
		return nil
	}
	return store
}

type usageQueryBody struct {
	FromMS       int64    `json:"from_ms"`
	ToMS         int64    `json:"to_ms"`
	Search       string   `json:"search"`
	Models       []string `json:"models"`
	Providers    []string `json:"providers"`
	AuthIndices  []string `json:"auth_indices"`
	Sources      []string `json:"sources"`
	APIKeys      []string `json:"api_keys"`
	APIKeyHashes []string `json:"api_key_hashes"`
	FailedOnly   bool     `json:"failed_only"`
	SuccessOnly  bool     `json:"success_only"`
	Limit        int      `json:"limit"`
	BeforeID     int64    `json:"before_id"`
	Fields       []string `json:"fields"`
}

func parseUsageFilter(c *gin.Context, body *usageQueryBody) usagestore.QueryFilter {
	filter := usagestore.QueryFilter{}
	if body != nil {
		filter.FromMS = body.FromMS
		filter.ToMS = body.ToMS
		filter.Search = body.Search
		filter.Models = body.Models
		filter.Providers = body.Providers
		filter.AuthIndices = body.AuthIndices
		filter.Sources = body.Sources
		filter.APIKeys = body.APIKeys
		filter.APIKeyHashes = body.APIKeyHashes
		filter.FailedOnly = body.FailedOnly
		filter.SuccessOnly = body.SuccessOnly
		filter.Limit = body.Limit
		filter.BeforeID = body.BeforeID
	}
	// Query string overrides for GET convenience.
	if v := c.Query("from_ms"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			filter.FromMS = n
		}
	}
	if v := c.Query("to_ms"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			filter.ToMS = n
		}
	}
	if v := c.Query("search"); v != "" {
		filter.Search = v
	}
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			filter.Limit = n
		}
	}
	if v := c.Query("before_id"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			filter.BeforeID = n
		}
	}
	if v := c.Query("failed_only"); v == "1" || strings.EqualFold(v, "true") {
		filter.FailedOnly = true
	}
	if v := c.Query("success_only"); v == "1" || strings.EqualFold(v, "true") {
		filter.SuccessOnly = true
	}
	if v := c.Query("models"); v != "" {
		filter.Models = splitCSV(v)
	}
	if v := c.Query("providers"); v != "" {
		filter.Providers = splitCSV(v)
	}
	if v := c.Query("auth_indices"); v != "" {
		filter.AuthIndices = splitCSV(v)
	}
	if v := c.Query("sources"); v != "" {
		filter.Sources = splitCSV(v)
	}
	if v := c.Query("api_keys"); v != "" {
		filter.APIKeys = splitCSV(v)
	}
	if v := c.Query("api_key_hashes"); v != "" {
		filter.APIKeyHashes = splitCSV(v)
	}
	return filter
}

func splitCSV(v string) []string {
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (h *Handler) pricingMaps(c *gin.Context, store *usagestore.Store) (map[string]usagestore.ModelPrice, map[string]string, bool) {
	prices, err := store.LoadModelPrices(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return nil, nil, false
	}
	aliases, err := store.LoadModelPriceAliases(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return nil, nil, false
	}
	return prices, usagestore.AliasMap(aliases), true
}

// GetUsageEvents lists durable usage events (newest first).
func (h *Handler) GetUsageEvents(c *gin.Context) {
	store := h.requireUsageStore(c)
	if store == nil {
		return
	}
	var body usageQueryBody
	if c.Request.Method == http.MethodPost {
		_ = c.ShouldBindJSON(&body)
	}
	filter := parseUsageFilter(c, &body)
	events, err := store.ListEvents(c.Request.Context(), filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	prices, aliases, ok := h.pricingMaps(c, store)
	if !ok {
		return
	}
	usagestore.AttachEventCosts(events, prices, aliases)
	var nextBeforeID int64
	if len(events) > 0 {
		nextBeforeID = events[len(events)-1].ID
	}
	c.JSON(http.StatusOK, gin.H{
		"events":          events,
		"next_before_id":  nextBeforeID,
		"generated_at_ms": time.Now().UnixMilli(),
		"store_path":      store.Path(),
	})
}

// GetUsageSummary returns aggregate metrics for a range.
func (h *Handler) GetUsageSummary(c *gin.Context) {
	store := h.requireUsageStore(c)
	if store == nil {
		return
	}
	var body usageQueryBody
	if c.Request.Method == http.MethodPost {
		_ = c.ShouldBindJSON(&body)
	}
	filter := parseUsageFilter(c, &body)
	summary, err := store.GetSummary(c.Request.Context(), filter)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Cost estimate across ALL matching events, aggregated per model in SQL so
	// the total covers every row (not just a capped page) and stays consistent
	// with per-model filtered totals.
	prices, aliases, ok := h.pricingMaps(c, store)
	if !ok {
		return
	}
	if total, priced, err := store.SumCost(c.Request.Context(), filter, prices, aliases); err == nil {
		summary.EstimatedCost = total
		summary.PricedCalls = priced
	}

	c.JSON(http.StatusOK, gin.H{
		"summary":                  summary,
		"generated_at_ms":          time.Now().UnixMilli(),
		"usage_statistics_enabled": h.cfg != nil && h.cfg.UsageStatisticsEnabled,
	})
}

// GetUsageFilterOptions returns distinct filter values.
func (h *Handler) GetUsageFilterOptions(c *gin.Context) {
	store := h.requireUsageStore(c)
	if store == nil {
		return
	}
	var body usageQueryBody
	if c.Request.Method == http.MethodPost {
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid usage query"})
			return
		}
	}
	filter := parseUsageFilter(c, &body)
	if v := c.Query("fields"); v != "" {
		body.Fields = splitCSV(v)
	}
	for _, field := range body.Fields {
		switch field {
		case "models", "providers", "auth_indices", "sources", "api_keys", "api_key_hashes":
		default:
			c.JSON(http.StatusBadRequest, gin.H{"error": "unknown filter field", "field": field})
			return
		}
	}
	opts, err := store.GetFilterOptions(c.Request.Context(), filter, body.Fields...)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, opts)
}

// GetUsageAccountStats returns per-account aggregates.
func (h *Handler) GetUsageAccountStats(c *gin.Context) {
	store := h.requireUsageStore(c)
	if store == nil {
		return
	}
	var body usageQueryBody
	if c.Request.Method == http.MethodPost {
		_ = c.ShouldBindJSON(&body)
	}
	filter := parseUsageFilter(c, &body)
	limit := 100
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	stats, err := store.GetAccountStats(c.Request.Context(), filter, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	prices, aliases, ok := h.pricingMaps(c, store)
	if !ok {
		return
	}
	// Aggregate cost per account across ALL matching events (per-model pricing),
	// instead of bucketing a capped page of recent events.
	if costByKey, err := store.CostByAccount(c.Request.Context(), filter, prices, aliases); err == nil {
		for i := range stats {
			key := usagestore.AccountKey(stats[i].AuthIndex, stats[i].Source, stats[i].SourceHash, stats[i].Provider)
			stats[i].EstimatedCost = costByKey[key]
		}
	}
	c.JSON(http.StatusOK, gin.H{"accounts": stats, "generated_at_ms": time.Now().UnixMilli()})
}

// GetUsageAPIKeyStats returns per-client-API-key aggregates (spending by key).
func (h *Handler) GetUsageAPIKeyStats(c *gin.Context) {
	store := h.requireUsageStore(c)
	if store == nil {
		return
	}
	var body usageQueryBody
	if c.Request.Method == http.MethodPost {
		_ = c.ShouldBindJSON(&body)
	}
	filter := parseUsageFilter(c, &body)
	limit := 100
	if v := c.Query("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	stats, err := store.GetAPIKeyStats(c.Request.Context(), filter, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	prices, aliases, ok := h.pricingMaps(c, store)
	if !ok {
		return
	}
	if costByKey, err := store.CostByAPIKey(c.Request.Context(), filter, prices, aliases); err == nil {
		for i := range stats {
			key := usagestore.APIKeyGroupKey(stats[i].APIKey, stats[i].APIKeyHash)
			stats[i].EstimatedCost = costByKey[key]
		}
	}
	c.JSON(http.StatusOK, gin.H{"api_keys": stats, "generated_at_ms": time.Now().UnixMilli()})
}

// GetModelPrices returns price book + aliases + unpriced models helper.
func (h *Handler) GetModelPrices(c *gin.Context) {
	store := h.requireUsageStore(c)
	if store == nil {
		return
	}
	prices, err := store.LoadModelPrices(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	aliases, err := store.LoadModelPriceAliases(c.Request.Context())
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	// Include every model that has usage records (not only recent window).
	models, _ := store.ListDistinctModels(c.Request.Context(), 0, 2000)
	aliasMap := usagestore.AliasMap(aliases)
	unpriced := make([]string, 0)
	for _, m := range models {
		if _, _, ok := usagestore.ResolvePrice([]string{m}, prices, aliasMap); !ok {
			unpriced = append(unpriced, m)
		}
	}
	list := make([]usagestore.ModelPrice, 0, len(prices))
	for _, p := range prices {
		list = append(list, p)
	}
	c.JSON(http.StatusOK, gin.H{
		"prices":          list,
		"aliases":         aliases,
		"unpriced_models": unpriced,
		"store_path":      store.Path(),
	})
}

// PutModelPrices upserts model prices. Body: { "prices": [...], "replace": false }
func (h *Handler) PutModelPrices(c *gin.Context) {
	store := h.requireUsageStore(c)
	if store == nil {
		return
	}
	var body struct {
		Prices  []usagestore.ModelPrice `json:"prices"`
		Replace bool                    `json:"replace"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if err := usagestore.ValidateModelPrices(body.Prices); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	var err error
	if body.Replace {
		err = store.ReplaceModelPrices(c.Request.Context(), body.Prices)
	} else {
		err = store.UpsertModelPrices(c.Request.Context(), body.Prices)
	}
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "count": len(body.Prices)})
}

// PutModelPriceAliases upserts aliases. Body: { "aliases": [...] }
func (h *Handler) PutModelPriceAliases(c *gin.Context) {
	store := h.requireUsageStore(c)
	if store == nil {
		return
	}
	var body struct {
		Aliases []usagestore.ModelPriceAlias `json:"aliases"`
	}
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
		return
	}
	if err := store.UpsertModelPriceAliases(c.Request.Context(), body.Aliases); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true, "count": len(body.Aliases)})
}

// DeleteModelPriceAlias deletes one alias: ?alias=
func (h *Handler) DeleteModelPriceAlias(c *gin.Context) {
	store := h.requireUsageStore(c)
	if store == nil {
		return
	}
	alias := strings.TrimSpace(c.Query("alias"))
	if alias == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "alias required"})
		return
	}
	if err := store.DeleteModelPriceAlias(c.Request.Context(), alias); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// DeleteModelPrice deletes one price: ?model=
func (h *Handler) DeleteModelPrice(c *gin.Context) {
	store := h.requireUsageStore(c)
	if store == nil {
		return
	}
	model := strings.TrimSpace(c.Query("model"))
	if model == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "model required"})
		return
	}
	if err := store.DeleteModelPrice(c.Request.Context(), model); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

// PostModelPricesSync fetches models.dev, LiteLLM, and OpenRouter catalogs, auto-imports
// exact/unique matches for models seen in usage (or body.models), and returns
// fuzzy candidates that need user confirmation.
//
// Body (all optional):
//
//	{
//	  "models": ["optional", "override", "list"],
//	  "override_manual": false,
//	  "apply_matched": true
//	}
//
// apply_matched defaults to true when omitted.
func (h *Handler) PostModelPricesSync(c *gin.Context) {
	store := h.requireUsageStore(c)
	if store == nil {
		return
	}
	var body struct {
		Models         []string `json:"models"`
		OverrideManual bool     `json:"override_manual"`
		ApplyMatched   *bool    `json:"apply_matched"`
	}
	if c.Request.ContentLength != 0 {
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
	}
	apply := true
	if body.ApplyMatched != nil {
		apply = *body.ApplyMatched
	}
	result, err := store.SyncModelPrices(c.Request.Context(), usagestore.PriceSyncRequest{
		Models:         body.Models,
		OverrideManual: body.OverrideManual,
		ApplyMatched:   apply,
	})
	if err != nil {
		c.JSON(http.StatusBadGateway, gin.H{
			"error": err.Error(), "source_results": result.SourceResults,
			"outcomes": result.Outcomes, "preserved": result.Preserved,
		})
		return
	}
	c.JSON(http.StatusOK, result)
}

// GetUsageAccountRecentRequests serves bounded status-bar data without full account statistics.
func (h *Handler) GetUsageAccountRecentRequests(c *gin.Context) {
	store := h.requireUsageStore(c)
	if store == nil {
		return
	}
	var body struct {
		usageQueryBody
		Accounts []usagestore.AccountRecentRequests `json:"accounts"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 1<<20)
	if err := c.ShouldBindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid recent-request query"})
		return
	}
	filter := parseUsageFilter(c, &body.usageQueryBody)
	if len(body.Accounts) > 200 || filter.FromMS < 0 || filter.ToMS < 0 || (filter.FromMS > 0 && filter.ToMS > 0 && filter.FromMS > filter.ToMS) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid time range or more than 200 account groups"})
		return
	}
	now := time.Now()
	accounts, err := store.GetAccountRecentRequests(c.Request.Context(), filter, body.Accounts, now)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"accounts": accounts, "generated_at_ms": now.UnixMilli()})
}
