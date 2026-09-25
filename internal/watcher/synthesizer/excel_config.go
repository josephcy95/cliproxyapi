package synthesizer

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/excel"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// excelProvider is the provider key bound to the Excel executor.
const excelProvider = "excel"

// synthesizeExcelKeys creates Auth entries for the ChatGPT Excel provider.
//
// Unlike the other providers this one is not keyed by an API key: the backend is
// reached with a ChatGPT OAuth access token. An explicit access-token entry
// produces a standalone credential; otherwise the provider is registered once
// so that loaded Codex accounts with matching plan access can serve it.
func (s *ConfigSynthesizer) synthesizeExcelKeys(ctx *SynthesisContext) []*coreauth.Auth {
	cfg := ctx.Config
	now := ctx.Now
	idGen := ctx.IDGenerator

	entries := cfg.ExcelKey
	if len(entries) == 0 {
		return nil
	}

	out := make([]*coreauth.Auth, 0, len(entries))
	for index := range entries {
		entry := entries[index]
		if entry.Disabled {
			continue
		}
		token := strings.TrimSpace(entry.AccessToken)
		accountID := strings.TrimSpace(entry.AccountID)
		base := strings.TrimSpace(entry.BaseURL)
		if token == "" && !entry.UseCodexAuths {
			// Nothing to authenticate with and Codex accounts were not opted in.
			continue
		}
		if token != "" && accountID == "" {
			// The backend rejects a request without a ChatGPT account id, so an
			// incomplete entry is skipped rather than failing at request time.
			continue
		}

		id, fingerprint := idGen.Next(
			excelProvider+":credential",
			token,
			accountID,
			base,
			config.FormatSortedHeaders(entry.Headers),
		)
		attrs := map[string]string{
			"source":       fmt.Sprintf("config:%s[%s]", excelProvider, fingerprint),
			"config_index": strconv.Itoa(index),
		}
		if token != "" {
			attrs["api_key"] = token
		}
		if accountID != "" {
			attrs["account_id"] = accountID
		}
		if base != "" {
			attrs["base_url"] = base
		}
		metadata := map[string]any{}
		if entry.UseCodexAuths {
			// Borrow the token from the loaded Codex accounts at request time
			// instead of copying it into configuration, so there is only ever one
			// refresh lifecycle for the ChatGPT token.
			metadata["use_codex_auths"] = true
			if accountID != "" {
				metadata["preferred_account_id"] = accountID
			}
		}
		if entry.DisableCooling != nil {
			metadata["disable_cooling"] = *entry.DisableCooling
		}
		addRequestRetryToMetadata(entry.RequestRetry, metadata)
		addConfigHeadersToAttrs(entry.Headers, attrs)

		out = append(out, &coreauth.Auth{
			ID:         id,
			Provider:   excelProvider,
			Label:      excelProvider + "-credential",
			Status:     coreauth.StatusActive,
			Attributes: attrs,
			Metadata:   metadata,
			CreatedAt:  now,
			UpdatedAt:  now,
		})
	}
	return out
}

// resolveExcelModels returns the configured alias list, falling back to the
// built-in defaults when configuration declares none.
func resolveExcelModels(entry *config.ExcelKey) []config.ExcelModel {
	if entry != nil && len(entry.Models) > 0 {
		return entry.Models
	}
	models := make([]config.ExcelModel, 0, len(excel.DefaultModels))
	for _, model := range excel.DefaultModels {
		models = append(models, config.ExcelModel{
			Name:             model.Slug,
			Alias:            model.Alias,
			DisplayName:      model.DisplayName,
			MaxContextLength: model.ContextLength,
		})
	}
	return models
}
