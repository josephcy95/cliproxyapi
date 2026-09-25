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
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
)

// ExcelExecutor serves Codex Responses traffic from the OpenAI backend used by
// the official ChatGPT Excel add-in.
//
// The backend authenticates the same ChatGPT/Codex OAuth token family as the
// public Codex endpoint, but it does not apply that endpoint's automatic model
// routing, which is the reason for using it. It is not wire-compatible with the
// Codex Responses API, so this executor owns the full translation rather than
// delegating to the shared Codex executor: the request is rebuilt in the
// add-in's shape (see internal/excel), the client's tools are relayed through a
// text-marker protocol because the backend rejects a declared "tools" field,
// and the stream is rewritten back into Responses events.
//
// It deliberately does not use the shared Codex request pipeline. That pipeline
// injects an image-generation tool the backend cannot serve, rewrites
// instructions and tool schemas, and drops long-id reasoning items in ways that
// are correct for the public endpoint but wrong here.
type ExcelExecutor struct {
	cfg *config.Config
}

// NewExcelExecutor creates an executor bound to the Excel provider.
func NewExcelExecutor(cfg *config.Config) *ExcelExecutor {
	return &ExcelExecutor{cfg: cfg}
}

// Identifier implements cliproxyauth.ProviderExecutor.
func (e *ExcelExecutor) Identifier() string { return helps.ExcelProvider }

// RequestToFormat keeps the payload in the client's Responses format.
//
// Because the executor round-trips Responses to Responses, no registered
// request translator applies and the body reaches Execute verbatim. That is what
// avoids the public Codex pipeline's rewrites.
func (e *ExcelExecutor) RequestToFormat(_ cliproxyexecutor.Request, _ cliproxyexecutor.Options) sdktranslator.Format {
	return sdktranslator.FormatOpenAIResponse
}

// PrepareRequest injects the credential and add-in identity headers.
func (e *ExcelExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	credential, okCredential := resolveExcelCredential(req.Context(), auth)
	if !okCredential || credential.AccessToken == "" {
		return statusErr{
			code: http.StatusUnauthorized,
			msg: "excel executor: no ChatGPT credential available; configure excel-api-key with access-token/account-id, " +
				"or set use-codex-auths with a loaded Codex account",
		}
	}
	req.Header.Set("Authorization", "Bearer "+credential.AccessToken)
	if credential.AccountID != "" {
		req.Header.Set("Chatgpt-Account-Id", credential.AccountID)
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

// HttpRequest injects credentials and executes the request.
func (e *ExcelExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("excel executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if errPrepare := e.PrepareRequest(httpReq, auth); errPrepare != nil {
		return nil, errPrepare
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// Refresh is a no-op: the Excel backend is reached with the same ChatGPT OAuth
// token the Codex provider refreshes, so there is no separate refresh flow.
func (e *ExcelExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	return auth, nil
}

// CountTokens is not supported: the backend reports usage itself and does not
// expose a tokenizer.
func (e *ExcelExecutor) CountTokens(_ context.Context, _ *cliproxyauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, statusErr{code: http.StatusNotImplemented, msg: "excel executor: token counting is not supported"}
}

// Execute serves a non-streaming client request by draining the upstream stream
// and returning the transformed terminal payload.
func (e *ExcelExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	plan, errPlan := e.prepare(ctx, auth, req, opts)
	if errPlan != nil {
		err = errPlan
		return resp, err
	}

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, errDo := e.send(ctx, httpClient, auth, plan, false)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		err = errDo
		return resp, err
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("excel executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if errStatus := e.statusError(ctx, httpResp); errStatus != nil {
		err = errStatus
		return resp, err
	}

	// The backend always answers with an SSE stream, even for a non-streaming
	// caller, so the terminal payload has to be recovered from the events.
	transform := excel.NewTransform(plan.catalog, plan.clientModel)
	reader := bufio.NewReaderSize(httpResp.Body, 64*1024)
	for {
		chunk, errRead := reader.ReadBytes('\n')
		if len(chunk) > 0 {
			transform.Write(chunk)
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
		err = statusErr{code: http.StatusBadGateway, msg: "excel executor: upstream response did not include a completed response payload"}
		return resp, err
	}
	encoded, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		err = fmt.Errorf("excel executor: marshal response failed: %w", errMarshal)
		return resp, err
	}
	e.publishUsage(ctx, reporter, payload)
	resp = cliproxyexecutor.Response{Payload: encoded, Headers: httpResp.Header.Clone()}
	return resp, nil
}

// ExecuteStream serves a streaming client request, rewriting upstream events
// into the Responses events the client expects.
func (e *ExcelExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	plan, errPlan := e.prepare(ctx, auth, req, opts)
	if errPlan != nil {
		err = errPlan
		return nil, err
	}

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpResp, errDo := e.send(ctx, httpClient, auth, plan, true)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		err = errDo
		return nil, err
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if errStatus := e.statusError(ctx, httpResp); errStatus != nil {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("excel executor: close response body error: %v", errClose)
		}
		err = errStatus
		return nil, err
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("excel executor: close response body error: %v", errClose)
			}
		}()

		transform := excel.NewTransform(plan.catalog, plan.clientModel)
		reader := bufio.NewReaderSize(httpResp.Body, 64*1024)
		for {
			chunk, errRead := reader.ReadBytes('\n')
			if len(chunk) > 0 {
				for _, event := range transform.Write(chunk) {
					select {
					case out <- cliproxyexecutor.StreamChunk{Payload: event}:
					case <-ctx.Done():
						return
					}
				}
			}
			if errRead != nil {
				if errRead == io.EOF {
					break
				}
				select {
				case out <- cliproxyexecutor.StreamChunk{Err: errRead}:
				case <-ctx.Done():
				}
				return
			}
		}
		for _, event := range transform.Close() {
			select {
			case out <- cliproxyexecutor.StreamChunk{Payload: event}:
			case <-ctx.Done():
				return
			}
		}
		if payload := transform.Terminal(); payload != nil {
			e.publishUsage(ctx, reporter, payload)
		}
		reporter.EnsurePublished(ctx)
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// excelPlan carries the prepared upstream request.
type excelPlan struct {
	url         string
	body        []byte
	catalog     *excel.Catalog
	clientModel string
}

// prepare parses the client payload and builds the add-in shaped request.
func (e *ExcelExecutor) prepare(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (*excelPlan, error) {
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "excel executor: /responses/compact is not supported"}
	}
	clientModel := strings.TrimSpace(req.Model)
	if clientModel == "" {
		clientModel = strings.TrimSpace(thinking.ParseSuffix(req.Model).ModelName)
	}
	if _, ok := excel.SlugFor(clientModel); !ok {
		return nil, statusErr{code: http.StatusBadRequest, msg: fmt.Sprintf("excel executor: model %q is not an Excel alias", clientModel)}
	}

	var decoded map[string]any
	if errUnmarshal := json.Unmarshal(req.Payload, &decoded); errUnmarshal != nil {
		return nil, statusErr{code: http.StatusBadRequest, msg: fmt.Sprintf("excel executor: invalid request payload: %v", errUnmarshal)}
	}
	// The provider is Responses-only: the backend speaks the add-in's Responses
	// dialect and no other translator targets it, so a caller speaking another
	// protocol would otherwise be handed a confusing parse failure.
	if _, hasInput := decoded["input"]; !hasInput {
		return nil, statusErr{
			code: http.StatusBadRequest,
			msg:  "excel executor: the Excel provider requires the OpenAI Responses API (/v1/responses); other client protocols are not supported",
		}
	}

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
		return nil, fmt.Errorf("excel executor: marshal upstream body failed: %w", errMarshal)
	}

	baseURL := strings.TrimSpace(excelBaseURL(auth))
	if baseURL == "" {
		baseURL = excel.DefaultBaseURL
	}
	return &excelPlan{
		url:         strings.TrimSuffix(baseURL, "/") + excel.ResponsesPath,
		body:        encoded,
		catalog:     catalog,
		clientModel: clientModel,
	}, nil
}

// send issues the upstream request.
func (e *ExcelExecutor) send(ctx context.Context, client *http.Client, auth *cliproxyauth.Auth, plan *excelPlan, stream bool) (*http.Response, error) {
	httpReq, errRequest := http.NewRequestWithContext(ctx, http.MethodPost, plan.url, bytes.NewReader(plan.body))
	if errRequest != nil {
		return nil, errRequest
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("Accept-Encoding", "identity")
	if errPrepare := e.PrepareRequest(httpReq, auth); errPrepare != nil {
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

// statusError converts a non-2xx upstream response into an error.
func (e *ExcelExecutor) statusError(ctx context.Context, httpResp *http.Response) error {
	if httpResp.StatusCode >= 200 && httpResp.StatusCode < 300 {
		return nil
	}
	// An authentication failure means the borrowed Codex token was stale or
	// revoked. Drop the cached copy so the next attempt re-reads the auth files
	// and picks up whatever the Codex provider refreshed in the meantime.
	if httpResp.StatusCode == http.StatusUnauthorized || httpResp.StatusCode == http.StatusForbidden {
		invalidateCodexCredentialCache()
	}
	body, _ := io.ReadAll(httpResp.Body)
	helps.AppendAPIResponseChunk(ctx, e.cfg, body)
	helps.LogWithRequestID(ctx).Debugf("excel executor: request error, status=%d message=%s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), body))
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
		return "excel executor: " + decoded.Error.Message
	}
	if text := strings.TrimSpace(string(body)); text != "" {
		if len(text) > 300 {
			text = text[:300]
		}
		return fmt.Sprintf("excel executor: upstream status %d: %s", status, text)
	}
	return fmt.Sprintf("excel executor: upstream status %d", status)
}

// publishUsage forwards the backend's token accounting to the usage reporter.
func (e *ExcelExecutor) publishUsage(ctx context.Context, reporter *helps.UsageReporter, payload map[string]any) {
	rawUsage, okUsage := payload["usage"].(map[string]any)
	if !okUsage {
		return
	}
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
	reporter.Publish(ctx, detail)
}

// excelBaseURL resolves the upstream base URL for the selected credential.
func excelBaseURL(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Attributes != nil {
		if value := strings.TrimSpace(auth.Attributes["base_url"]); value != "" {
			return value
		}
	}
	if auth.Metadata != nil {
		if value, okValue := auth.Metadata["base_url"].(string); okValue && strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}
