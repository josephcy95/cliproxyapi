package executor

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/excel"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// This file serves the Excel-backed model aliases from the Codex executor.
//
// The models are Codex models for every purpose that matters here: they are
// selected, cooled down, attributed in usage and reported in the model list
// exactly like any other Codex model, using the same credential the scheduler
// picked. Only the upstream conversation differs, because the Excel add-in
// backend speaks its own dialect and does not apply the public endpoint's
// automatic model routing.
//
// It therefore deliberately does not go through the shared Codex request
// pipeline: that pipeline injects an image-generation tool the backend cannot
// serve, rewrites instructions and tool schemas, and drops long-id reasoning
// items, all of which are correct for the public endpoint but wrong here. See
// internal/excel for the protocol itself. Client protocol translation still uses
// the shared registry, before and after this backend-specific adapter.

// excelPlan carries the prepared upstream request.
type excelPlan struct {
	url            string
	body           []byte
	catalog        *excel.Catalog
	clientModel    string
	responseFormat sdktranslator.Format
	original       []byte
	translated     []byte
}

// executeExcel serves a non-streaming client request from the Excel backend.
func (e *CodexExecutor) executeExcel(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, baseModel string) (resp cliproxyexecutor.Response, err error) {
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	plan, errPlan := e.prepareExcelPlan(ctx, auth, req, opts, baseModel)
	if errPlan != nil {
		err = errPlan
		return resp, err
	}

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, errDo := e.sendExcel(ctx, httpClient, auth, plan)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		err = errDo
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex excel: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if errStatus := excelStatusError(ctx, e.cfg, httpResp); errStatus != nil {
		err = errStatus
		return resp, err
	}

	// The backend always answers with an SSE stream, even for a non-streaming
	// caller, so the terminal payload is recovered from the events.
	transform := excel.NewTransform(plan.catalog, plan.clientModel)
	reader := bufio.NewReaderSize(httpResp.Body, 64*1024)
	for {
		chunk, errRead := reader.ReadBytes('\n')
		if len(chunk) > 0 {
			transform.Write(chunk)
			if transform.Terminal() != nil {
				break
			}
		}
		if errRead != nil {
			if errRead != io.EOF {
				err = errRead
				return resp, err
			}
			break
		}
	}
	transform.Close()

	payload := transform.Terminal()
	if payload == nil {
		err = statusErr{code: http.StatusBadGateway, msg: "codex excel: upstream response did not include a completed response payload"}
		return resp, err
	}
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		err = fmt.Errorf("codex excel: marshal response failed: %w", errMarshal)
		return resp, err
	}
	publishExcelUsage(ctx, reporter, payload)
	terminal, _ := json.Marshal(map[string]any{"type": "response." + fmt.Sprint(payload["status"]), "response": payload})
	if failure, _, ok := codexTerminalFailureErr(terminal); ok {
		return resp, failure
	}
	if plan.responseFormat != sdktranslator.FormatOpenAIResponse && plan.responseFormat != sdktranslator.FormatCodex {
		var param any
		encoded = sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatCodex, plan.responseFormat, plan.clientModel, plan.original, plan.translated, terminal, &param)
	}
	resp = cliproxyexecutor.Response{Payload: encoded, Headers: httpResp.Header.Clone()}
	return resp, nil
}

// executeExcelStream serves a streaming client request from the Excel backend.
func (e *CodexExecutor) executeExcelStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, baseModel string) (_ *cliproxyexecutor.StreamResult, err error) {
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	plan, errPlan := e.prepareExcelPlan(ctx, auth, req, opts, baseModel)
	if errPlan != nil {
		err = errPlan
		return nil, err
	}

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, errDo := e.sendExcel(ctx, httpClient, auth, plan)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		err = errDo
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if errStatus := excelStatusError(ctx, e.cfg, httpResp); errStatus != nil {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("codex excel: close response body error: %v", errClose)
		}
		err = errStatus
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		// The outer function has returned by now, so it cannot track errors
		// from this goroutine. Keep the stream outcome local to avoid a race.
		var streamErr error
		defer reporter.TrackFailure(ctx, &streamErr)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("codex excel: close response body error: %v", errClose)
			}
		}()

		transform := excel.NewTransform(plan.catalog, plan.clientModel)
		var param any
		forward := func(events [][]byte) bool {
			// Account before exposing completion: the client may disconnect as
			// soon as it receives the terminal frame, and EOF may never arrive.
			if payload := transform.Terminal(); payload != nil {
				publishExcelUsage(ctx, reporter, payload)
			}
			for _, event := range events {
				chunks := [][]byte{event}
				if plan.responseFormat != sdktranslator.FormatOpenAIResponse && plan.responseFormat != sdktranslator.FormatCodex {
					chunks = nil
					// Transform emits complete SSE frames; shared translators consume data lines.
					for _, line := range bytes.Split(event, []byte("\n")) {
						if !bytes.HasPrefix(line, []byte("data:")) {
							continue
						}
						data := bytes.TrimSpace(line[5:])
						if failure, _, ok := codexTerminalFailureErr(data); ok {
							streamErr = failure
							select {
							case out <- cliproxyexecutor.StreamChunk{Err: failure}:
							case <-ctx.Done():
							}
							return false
						}
						chunks = append(chunks, sdktranslator.TranslateStream(ctx, sdktranslator.FormatCodex, plan.responseFormat, plan.clientModel, plan.original, plan.translated, line, &param)...)
					}
				}
				for _, chunk := range chunks {
					select {
					case out <- cliproxyexecutor.StreamChunk{Payload: chunk}:
					case <-ctx.Done():
						streamErr = ctx.Err()
						return false
					}
				}
			}
			return true
		}
		reader := bufio.NewReaderSize(httpResp.Body, 64*1024)
		for {
			chunk, errRead := reader.ReadBytes('\n')
			if len(chunk) > 0 {
				if !forward(transform.Write(chunk)) {
					return
				}
				if transform.Terminal() != nil {
					return
				}
			}
			if errRead == nil {
				continue
			}
			if errRead == io.EOF {
				if !forward(transform.Close()) || transform.Terminal() != nil {
					return
				}
				streamErr = newCodexIncompleteStreamError()
			} else {
				streamErr = errRead
			}
			helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
			reporter.PublishFailure(ctx, streamErr)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
			case <-ctx.Done():
			}
			return
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// prepareExcelPlan parses the client payload and builds the add-in shaped request.
func (e *CodexExecutor) prepareExcelPlan(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, baseModel string) (*excelPlan, error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "codex executor: /responses/compact is not supported for Excel-backed models"}
	}
	clientModel := strings.TrimSpace(req.Model)
	if clientModel == "" {
		clientModel = baseModel
	}

	// Responses callers must bypass public-Codex normalizers to preserve their
	// custom tool schemas and Excel-specific reasoning items.
	from := opts.SourceFormat
	if from == "" {
		from = req.Format
	}
	if from == "" {
		from = sdktranslator.FormatOpenAIResponse
	}
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	if responseFormat == "" {
		responseFormat = from
	}
	original := opts.OriginalRequest
	if len(original) == 0 {
		original = req.Payload
	}
	if !json.Valid(req.Payload) {
		return nil, statusErr{code: http.StatusBadRequest, msg: "codex excel: invalid request JSON"}
	}
	body := req.Payload
	originalTranslated := original
	if from != sdktranslator.FormatOpenAIResponse && from != sdktranslator.FormatCodex {
		if !sdktranslator.HasRequestTransformer(from, sdktranslator.FormatCodex) {
			return nil, statusErr{code: http.StatusBadRequest, msg: fmt.Sprintf("codex excel: unsupported request format %q", from)}
		}
		originalTranslated, body = translateCodexRequestPair(from, sdktranslator.FormatCodex, baseModel, original, body, true)
	}
	if responseFormat != sdktranslator.FormatOpenAIResponse && responseFormat != sdktranslator.FormatCodex &&
		(!sdktranslator.HasStreamResponseTransformer(responseFormat, sdktranslator.FormatCodex) || !sdktranslator.HasNonStreamResponseTransformer(responseFormat, sdktranslator.FormatCodex)) {
		return nil, statusErr{code: http.StatusBadRequest, msg: fmt.Sprintf("codex excel: unsupported response format %q", responseFormat)}
	}
	var errThinking error
	body, errThinking = helps.ApplyRequestThinking(body, req, opts, from.String(), sdktranslator.FormatCodex.String(), e.Identifier())
	if errThinking != nil {
		return nil, errThinking
	}
	body = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, sdktranslator.FormatCodex.String(), from.String(), "", body, originalTranslated, helps.PayloadRequestedModel(opts, req.Model), helps.PayloadRequestPath(opts), opts.Headers)
	body = applyCodexConfiguredInstructions(e.cfg, auth, baseModel, body, opts)
	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(body, &decoded); errUnmarshal != nil || decoded == nil {
		return nil, statusErr{code: http.StatusBadRequest, msg: "codex excel: expected a request object"}
	}
	if _, hasInput := decoded["input"]; !hasInput {
		return nil, statusErr{code: http.StatusBadRequest, msg: "codex excel: translated request is missing input"}
	}
	// Routing has already resolved aliases and reasoning suffixes.
	decoded["model"] = baseModel
	decoded["stream"] = true
	body, _ = json.Marshal(decoded)

	var catalog *excel.Catalog
	if !excel.ToolChoiceDisabled(decoded) {
		catalog = excel.ParseCatalog(decoded["tools"])
	}
	if catalog == nil {
		catalog = &excel.Catalog{}
	}

	upstreamBody, errPrepare := excel.PrepareBody(decoded, catalog)
	if errPrepare != nil {
		return nil, statusErr{code: http.StatusBadRequest, msg: errPrepare.Error()}
	}
	encoded, errMarshal := json.Marshal(upstreamBody)
	if errMarshal != nil {
		return nil, fmt.Errorf("codex executor: marshal upstream body failed: %w", errMarshal)
	}

	baseURL := excelBaseURLFor(auth)
	if baseURL == "" {
		baseURL = excel.DefaultBaseURL
	}
	return &excelPlan{
		url:            strings.TrimSuffix(baseURL, "/") + excel.ResponsesPath,
		body:           encoded,
		catalog:        catalog,
		clientModel:    clientModel,
		responseFormat: responseFormat,
		original:       original,
		translated:     body,
	}, nil
}

// sendExcel issues the upstream request using the selected credential.
func (e *CodexExecutor) sendExcel(ctx context.Context, client *http.Client, auth *cliproxyauth.Auth, plan *excelPlan) (*http.Response, error) {
	httpReq, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, plan.url, bytes.NewReader(plan.body))
	if errRequest != nil {
		return nil, errRequest
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Accept-Encoding", "identity")
	if errPrepare := e.prepareExcelHeaders(httpReq, auth); errPrepare != nil {
		return nil, errPrepare
	}

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       plan.url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      plan.body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
	return client.Do(httpReq)
}

// prepareExcelHeaders applies the selected credential and the add-in identity
// headers the backend expects.
func (e *CodexExecutor) prepareExcelHeaders(req *http.Request, auth *cliproxyauth.Auth) error {
	apiKey, _ := codexCreds(auth)
	if strings.TrimSpace(apiKey) == "" {
		return statusErr{
			code: http.StatusUnauthorized,
			msg:  "codex excel: the selected credential has no access token",
		}
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	if accountID := excelAccountIDFor(auth); accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
	for name, value := range excel.DefaultHeaders() {
		if req.Header.Get(name) == "" {
			req.Header.Set(name, value)
		}
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// excelStatusError converts a non-2xx upstream response into an error.
func excelStatusError(ctx context.Context, cfg *config.Config, httpResp *http.Response) error {
	if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
		return nil
	}
	body, _ := io.ReadAll(httpResp.Body)
	helps.AppendAPIResponseChunk(ctx, cfg, body)
	helps.LogWithRequestID(ctx).Debugf("codex excel: request error, status=%d message=%s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), body))
	return statusErr{code: httpResp.StatusCode, msg: excelErrorMessage(httpResp.StatusCode, body)}
}

// excelErrorMessage extracts the backend's error message when present.
func excelErrorMessage(status int, body []byte) string {
	var decoded struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if errUnmarshal := json.Unmarshal(body, &decoded); errUnmarshal == nil && decoded.Error.Message != "" {
		return "codex excel: " + decoded.Error.Message
	}
	if text := strings.TrimSpace(string(body)); text != "" {
		if len(text) > 300 {
			text = text[:300]
		}
		return fmt.Sprintf("codex excel: upstream status %d: %s", status, text)
	}
	return fmt.Sprintf("codex excel: upstream status %d", status)
}

// publishExcelUsage forwards the backend's token accounting to the usage
// reporter, so these turns are counted like any other model's.
func publishExcelUsage(ctx context.Context, reporter *helps.UsageReporter, payload map[string]any) {
	// Missing token usage must not suppress request counting.
	rawUsage, _ := payload["usage"].(map[string]any)
	detail := usage.Detail{}
	if value, okValue := rawUsage["input_tokens"].(float64); okValue {
		detail.InputTokens = int64(value)
	}
	if value, okValue := rawUsage["output_tokens"].(float64); okValue {
		detail.OutputTokens = int64(value)
	}
	if value, okValue := rawUsage["total_tokens"].(float64); okValue {
		detail.TotalTokens = int64(value)
	}
	if details, okDetails := rawUsage["input_tokens_details"].(map[string]any); okDetails {
		if value, okValue := details["cached_tokens"].(float64); okValue {
			detail.CachedTokens = int64(value)
		}
	}
	if details, okDetails := rawUsage["output_tokens_details"].(map[string]any); okDetails {
		if value, okValue := details["reasoning_tokens"].(float64); okValue {
			detail.ReasoningTokens = int64(value)
		}
	}
	if payload["status"] == "failed" {
		encoded, errMarshal := json.Marshal(map[string]any{"type": "response.failed", "response": payload})
		if errMarshal != nil {
			reporter.PublishFailureWithDetail(ctx, detail, errMarshal)
			return
		}
		if failure, _, ok := codexTerminalFailureErr(encoded); ok {
			reporter.PublishFailureWithDetail(ctx, detail, failure)
			return
		}
	}
	reporter.Publish(ctx, detail)
}

// excelAccountIDFor resolves the ChatGPT account id for a credential.
func excelAccountIDFor(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if value := strings.TrimSpace(auth.Attributes["account_id"]); value != "" {
			return value
		}
	}
	if auth.Metadata != nil {
		if value, okValue := auth.Metadata["account_id"].(string); okValue && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

// excelBaseURLFor resolves an optional per-credential backend override.
func excelBaseURLFor(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if value := strings.TrimSpace(auth.Attributes["excel_base_url"]); value != "" {
			return value
		}
	}
	if auth.Metadata != nil {
		if value, okValue := auth.Metadata["excel_base_url"].(string); okValue && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
