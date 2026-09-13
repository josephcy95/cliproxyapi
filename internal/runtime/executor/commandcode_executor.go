package executor

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
)

const (
	// commandCodeProvider is the provider identifier used for registration,
	// routing, and usage reporting.
	commandCodeProvider = helps.CommandCodeProvider
	// commandCodeCLIPath is the CLI-mimic generation endpoint used by the
	// "Go" plan, which has no OpenAI-compatible surface.
	commandCodeCLIPath = "/alpha/generate"
	// commandCodeStreamBufferLimit bounds one upstream JSONL line.
	commandCodeStreamBufferLimit = 52_428_800
)

// CommandCodeExecutor serves Command Code accounts from static API keys. It
// mimics the vendor CLI: the nine-key envelope is posted to /alpha/generate,
// which is the only inference surface a "Go" plan can reach (a Go key is
// rejected with 403 upgrade_required by the public Provider API). Higher tiers
// are deliberately NOT handled here — they are served by the existing
// openai-compatibility provider pointed at /provider/v1. Requests always stream
// upstream; the upstream JSONL events are translated into OpenAI SSE frames
// that the SDK translators convert into the client's protocol.
type CommandCodeExecutor struct {
	cfg *config.Config
}

// NewCommandCodeExecutor creates an executor bound to the Command Code provider.
func NewCommandCodeExecutor(cfg *config.Config) *CommandCodeExecutor {
	return &CommandCodeExecutor{cfg: cfg}
}

// Identifier implements cliproxyauth.ProviderExecutor.
func (e *CommandCodeExecutor) Identifier() string { return commandCodeProvider }

// PrepareRequest injects Command Code credentials into the outgoing request.
func (e *CommandCodeExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	_, apiKey := e.resolveCredentials(auth)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Command Code credentials into the request and executes it.
func (e *CommandCodeExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("commandcode executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

// commandCodePreparedRequest is everything the execute paths need to talk to the
// upstream.
type commandCodePreparedRequest struct {
	url        string
	body       []byte
	translated []byte
	sessionID  string
}

// Execute serves a non-streaming client request. The upstream request always
// streams; the events are assembled into an OpenAI chat.completion payload and
// translated to the client protocol.
func (e *CommandCodeExecutor) Execute(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (resp cliproxyexecutor.Response, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	if opts.Alt == "responses/compact" {
		return resp, statusErr{code: http.StatusBadRequest, msg: "commandcode executor: /responses/compact is not supported"}
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	prepared, errPrepare := e.prepareUpstream(ctx, auth, req, opts, false, reporter)
	if errPrepare != nil {
		err = errPrepare
		return resp, err
	}

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpReq, errRequest := e.buildHTTPRequest(ctx, auth, opts, prepared)
	if errRequest != nil {
		err = errRequest
		return resp, err
	}
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		return resp, errDo
	}
	defer func() {
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("commandcode executor: close response body error: %v", errClose)
		}
	}()
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		err = commandCodeStatusError(httpResp.StatusCode, b)
		return resp, err
	}

	assembled, errConsume := e.consumeStream(ctx, httpResp.Body, prepared, req, opts, nil)
	if errConsume != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errConsume)
		return resp, errConsume
	}
	if detail, ok := assembled.UsageDetail(); ok {
		reporter.Publish(ctx, detail)
	}
	reporter.EnsurePublished(ctx)
	body := assembled.ChatCompletion()
	var param any
	out := sdktranslator.TranslateNonStream(ctx, sdktranslator.FormatOpenAI, responseFormat, req.Model, opts.OriginalRequest, prepared.translated, body, &param)
	if responseFormat == sdktranslator.FormatOpenAIResponse {
		out = helps.EnsureResponsesUsageDetails(out)
	}
	resp = cliproxyexecutor.Response{Payload: out, Headers: httpResp.Header.Clone()}
	return resp, nil
}

// ExecuteStream serves a streaming client request, translating each upstream
// event into client-protocol frames.
func (e *CommandCodeExecutor) ExecuteStream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (_ *cliproxyexecutor.StreamResult, err error) {
	ctx = helps.EnsureSessionContext(ctx, opts, req.Payload)
	if opts.Alt == "responses/compact" {
		return nil, statusErr{code: http.StatusBadRequest, msg: "commandcode executor: /responses/compact is not supported"}
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	reporter := helps.NewExecutorUsageReporter(ctx, e, baseModel, auth)
	defer reporter.TrackFailure(ctx, &err)

	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	prepared, errPrepare := e.prepareUpstream(ctx, auth, req, opts, true, reporter)
	if errPrepare != nil {
		return nil, errPrepare
	}

	httpClient := helps.NewProxyAwareHTTPClient(ctx, e.cfg, auth, 0)
	httpClient = reporter.TrackHTTPClient(httpClient)
	httpReq, errRequest := e.buildHTTPRequest(ctx, auth, opts, prepared)
	if errRequest != nil {
		return nil, errRequest
	}
	httpResp, errDo := httpClient.Do(httpReq)
	if errDo != nil {
		helps.RecordAPIResponseError(ctx, e.cfg, errDo)
		return nil, errDo
	}
	helps.RecordAPIResponseMetadata(ctx, e.cfg, httpResp.StatusCode, httpResp.Header.Clone())
	if httpResp.StatusCode < 200 || httpResp.StatusCode >= 300 {
		b, _ := io.ReadAll(httpResp.Body)
		if errClose := httpResp.Body.Close(); errClose != nil {
			log.Errorf("commandcode executor: close response body error: %v", errClose)
		}
		helps.AppendAPIResponseChunk(ctx, e.cfg, b)
		helps.LogWithRequestID(ctx).Debugf("request error, error status: %d, error message: %s", httpResp.StatusCode, helps.SummarizeErrorBody(httpResp.Header.Get("Content-Type"), b))
		return nil, commandCodeStatusError(httpResp.StatusCode, b)
	}

	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer func() {
			if errClose := httpResp.Body.Close(); errClose != nil {
				log.Errorf("commandcode executor: close response body error: %v", errClose)
			}
		}()

		var param any
		// Frames must reach the translators as SSE "data:" lines. The OpenAI
		// translator strips the prefix (the HTTP handler writes its own), while the
		// Claude/Gemini translators require it and silently drop bare JSON.
		emitTranslatedLine := func(frame []byte) bool {
			line := frame
			if !bytes.HasPrefix(frame, []byte("data:")) {
				line = make([]byte, 0, len(frame)+len(dataTag))
				line = append(line, dataTag...)
				line = append(line, frame...)
			}
			chunks := sdktranslator.TranslateStream(ctx, sdktranslator.FormatOpenAI, responseFormat, req.Model, opts.OriginalRequest, prepared.translated, line, &param)
			for i := range chunks {
				select {
				case out <- cliproxyexecutor.StreamChunk{Payload: chunks[i]}:
				case <-ctx.Done():
					return false
				}
			}
			return true
		}

		publishStreamError := func(streamErr error) {
			helps.RecordAPIResponseError(ctx, e.cfg, streamErr)
			reporter.PublishFailure(ctx, streamErr)
			select {
			case out <- cliproxyexecutor.StreamChunk{Err: streamErr}:
			case <-ctx.Done():
			}
		}

		assembled, errConsume := e.consumeStream(ctx, httpResp.Body, prepared, req, opts, emitTranslatedLine)
		if errConsume != nil {
			publishStreamError(errConsume)
			return
		}
		if assembled == nil {
			// The client disconnected while frames were being emitted.
			return
		}
		for _, frame := range assembled.TerminalFrames(!assembled.Finished()) {
			if !emitTranslatedLine(frame) {
				return
			}
		}
		if detail, ok := assembled.UsageDetail(); ok {
			reporter.Publish(ctx, detail)
		}
		reporter.EnsurePublished(ctx)
	}()
	return &cliproxyexecutor.StreamResult{Headers: httpResp.Header.Clone(), Chunks: out}, nil
}

// CountTokens counts the tokens of the translated OpenAI request.
func (e *CommandCodeExecutor) CountTokens(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	baseModel := thinking.ParseSuffix(req.Model).ModelName

	from := opts.SourceFormat
	responseFormat := cliproxyexecutor.ResponseFormatOrSource(opts)
	to := sdktranslator.FromString("openai")
	isCompat := helps.APIKeyModelIsCompat(req)
	translated := helps.TranslateRequestWithAPIKeyModelCompatibility(ctx, opts.Headers, e.cfg, from, to, baseModel, req.Payload, false, isCompat)

	translated, err := helps.ApplyRequestThinking(translated, req, opts, from.String(), to.String(), e.Identifier())
	if err != nil {
		return cliproxyexecutor.Response{}, err
	}

	enc, errTokenizer := helps.TokenizerForModel(baseModel)
	if errTokenizer != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("commandcode executor: tokenizer init failed: %w", errTokenizer)
	}
	count, errCount := helps.CountOpenAIChatTokens(enc, translated)
	if errCount != nil {
		return cliproxyexecutor.Response{}, fmt.Errorf("commandcode executor: token counting failed: %w", errCount)
	}
	usageJSON := helps.BuildOpenAIUsageJSON(count)
	translatedUsage := sdktranslator.TranslateTokenCount(ctx, to, responseFormat, count, usageJSON)
	return cliproxyexecutor.Response{Payload: translatedUsage}, nil
}

// Refresh is a no-op: Command Code credentials are long-lived static API keys
// with no refresh flow.
func (e *CommandCodeExecutor) Refresh(ctx context.Context, auth *cliproxyauth.Auth) (*cliproxyauth.Auth, error) {
	log.Debugf("commandcode executor: refresh called")
	if refreshed, handled, errRefresh := helps.RefreshAuthViaHome(ctx, e.cfg, auth); handled {
		return refreshed, errRefresh
	}
	return auth, nil
}

// prepareUpstream translates the client payload into OpenAI, resolves the
// upstream model and transport, and builds the request body.
func (e *CommandCodeExecutor) prepareUpstream(ctx context.Context, auth *cliproxyauth.Auth, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, stream bool, reporter *helps.UsageReporter) (*commandCodePreparedRequest, error) {
	attrBase, _ := e.resolveCredentials(auth)
	baseURL := commandCodeBaseURL(attrBase, e.resolveConfig(auth))
	if baseURL == "" {
		return nil, statusErr{code: http.StatusUnauthorized, msg: "missing provider baseURL"}
	}

	baseModel := thinking.ParseSuffix(req.Model).ModelName
	from := opts.SourceFormat
	to := sdktranslator.FromString("openai")
	originalPayload := req.Payload
	if len(opts.OriginalRequest) > 0 {
		originalPayload = opts.OriginalRequest
	}
	isCompat := helps.APIKeyModelIsCompat(req)
	originalTranslated := helps.TranslateRequestWithAPIKeyModelCompatibility(ctx, opts.Headers, e.cfg, from, to, baseModel, originalPayload, stream, isCompat)
	translated := helps.TranslateRequestWithAPIKeyModelCompatibility(ctx, opts.Headers, e.cfg, from, to, baseModel, req.Payload, stream, isCompat)

	translated, errThinking := helps.ApplyRequestThinking(translated, req, opts, from.String(), to.String(), e.Identifier())
	if errThinking != nil {
		return nil, errThinking
	}
	requestedModel := helps.PayloadRequestedModel(opts, req.Model)
	requestPath := helps.PayloadRequestPath(opts)
	translated = helps.ApplyPayloadConfigWithRequest(e.cfg, baseModel, to.String(), from.String(), "", translated, originalTranslated, requestedModel, requestPath, opts.Headers)
	if reporter != nil {
		reporter.SetTranslatedReasoningEffort(translated, to.String())
	}

	entry := e.resolveConfig(auth)
	sessionID := helps.CommandCodeSessionUUID(commandCodeAuthID(auth))
	upstreamModel := baseModel
	if entry != nil {
		upstreamModel = helps.CommandCodeModelUpstreamModel(entry.Models, baseModel, baseModel)
	}
	translated = helps.SetStringIfDifferent(translated, "model", upstreamModel)

	// The CLI transport streams unconditionally; a non-streaming client is
	// served by assembling the stream afterwards.
	envelope, errEnvelope := helps.BuildCommandCodeEnvelope(translated, helps.CommandCodeEnvelopeInput{
		Model:      upstreamModel,
		ThreadID:   sessionID,
		WorkingDir: helps.CommandCodeDefaultWorkingDir,
		Platform:   helps.CommandCodePlatform(),
	})
	if errEnvelope != nil {
		return nil, statusErr{code: http.StatusBadRequest, msg: errEnvelope.Error()}
	}

	return &commandCodePreparedRequest{
		url:        commandCodeEndpointURL(baseURL),
		body:       envelope,
		translated: translated,
		sessionID:  sessionID,
	}, nil
}

// buildHTTPRequest renders the prepared request into an HTTP request, applies
// the CLI header set, and records the upstream attempt.
func (e *CommandCodeExecutor) buildHTTPRequest(ctx context.Context, auth *cliproxyauth.Auth, opts cliproxyexecutor.Options, prepared *commandCodePreparedRequest) (*http.Request, error) {
	if prepared == nil {
		return nil, fmt.Errorf("commandcode executor: request was not prepared")
	}
	_, apiKey := e.resolveCredentials(auth)
	httpReq, errNewRequest := http.NewRequestWithContext(ctx, http.MethodPost, prepared.url, bytes.NewReader(prepared.body))
	if errNewRequest != nil {
		return nil, errNewRequest
	}
	e.applyHeaders(httpReq, e.resolveConfig(auth), apiKey, prepared.sessionID)
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(httpReq, attrs, opts.Headers)

	var authID, authLabel, authType, authValue string
	if auth != nil {
		authID = auth.ID
		authLabel = auth.Label
		authType, authValue = auth.AccountInfo()
	}
	helps.RecordAPIRequest(ctx, e.cfg, helps.UpstreamRequestLog{
		URL:       prepared.url,
		Method:    http.MethodPost,
		Headers:   httpReq.Header.Clone(),
		Body:      prepared.body,
		Provider:  e.Identifier(),
		AuthID:    authID,
		AuthLabel: authLabel,
		AuthType:  authType,
		AuthValue: authValue,
	})
	return httpReq, nil
}

// applyHeaders sets the CLI header set. It deliberately omits x-co-flag: the
// real CLI never sends it. Configuration headers are applied by the caller
// afterwards so they can override these defaults.
func (e *CommandCodeExecutor) applyHeaders(httpReq *http.Request, entry *config.CommandCodeKey, apiKey, sessionID string) {
	helps.ApplyCommandCodeHeaders(httpReq, entry, apiKey, sessionID)
}

// consumeStream reads the upstream JSONL stream, feeds every line to the
// translator, and forwards translated frames through emit when provided. The
// returned translator carries the assembled response and usage.
func (e *CommandCodeExecutor) consumeStream(ctx context.Context, body io.Reader, prepared *commandCodePreparedRequest, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, emit func([]byte) bool) (*helps.CommandCodeStreamTranslator, error) {
	assembled := helps.NewCommandCodeStreamTranslator(req.Model)
	scanner := bufio.NewScanner(body)
	scanner.Buffer(nil, commandCodeStreamBufferLimit)

	for scanner.Scan() {
		line := scanner.Bytes()
		helps.AppendAPIResponseChunk(ctx, e.cfg, line)
		frames, done, errFrame := assembled.Translate(line)
		if errFrame != nil {
			return assembled, errFrame
		}
		for _, frame := range frames {
			if emit != nil && !emit(frame) {
				return nil, nil
			}
		}
		if done {
			return assembled, nil
		}
	}
	if errScan := scanner.Err(); errScan != nil {
		return assembled, errScan
	}
	return assembled, nil
}

// commandCodeBaseURL resolves the upstream base URL. The auth attribute wins
// because it is what the synthesizer recorded from the config entry; the entry
// itself is the fallback, and the vendor host is the default like the CLI's own
// buildServerConfig.
func commandCodeBaseURL(baseURL string, entry *config.CommandCodeKey) string {
	if trimmed := strings.TrimSpace(baseURL); trimmed != "" {
		return trimmed
	}
	if entry != nil {
		if trimmed := strings.TrimSpace(entry.BaseURL); trimmed != "" {
			return trimmed
		}
	}
	return helps.CommandCodeDefaultBaseURL
}

// commandCodeStatusError builds the status error for a non-2xx upstream
// response, promoting billing exhaustion to its semantic status.
func commandCodeStatusError(httpStatus int, body []byte) statusErr {
	return statusErr{code: helps.NormalizeCommandCodeStatusError(httpStatus, body), msg: string(body)}
}

// resolveCredentials reads the base URL and API key from the auth attributes.
func (e *CommandCodeExecutor) resolveCredentials(auth *cliproxyauth.Auth) (baseURL, apiKey string) {
	if auth == nil {
		return "", ""
	}
	if auth.Attributes != nil {
		baseURL = strings.TrimSpace(auth.Attributes["base_url"])
		apiKey = strings.TrimSpace(auth.Attributes["api_key"])
	}
	return
}

// resolveConfig finds the Command Code config entry backing an auth. The
// synthesizer records the entry index and name; neither is the OpenAI-compat
// "compat_name" attribute, so these auths are not routed to the OpenAI-compat
// executor.
func (e *CommandCodeExecutor) resolveConfig(auth *cliproxyauth.Auth) *config.CommandCodeKey {
	if auth == nil || e.cfg == nil {
		return nil
	}
	if auth.AuthSourceKind() == cliproxyauth.AuthSourceConfig && auth.Attributes != nil {
		if rawIndex := strings.TrimSpace(auth.Attributes[cliproxyauth.AttributeConfigIndex]); rawIndex != "" {
			index, errIndex := strconv.Atoi(rawIndex)
			if errIndex == nil && index >= 0 && index < len(e.cfg.CommandCodeKey) {
				entry := &e.cfg.CommandCodeKey[index]
				// The index is only trusted when the entry still describes this
				// credential. A config reload can shift entries, and a stale auth
				// snapshot would otherwise make us read another credential's
				// models and headers for this request.
				if entry.APIKey != "" || entry.BaseURL != "" {
					if commandCodeEntryMatchesAuth(entry, auth) {
						return entry
					}
				}
			}
		}
	}
	// Fallback: match on the credential itself, the way other API-key families
	// resolve their config entry.
	_, apiKey := e.resolveCredentials(auth)
	baseURL, _ := e.resolveCredentials(auth)
	for i := range e.cfg.CommandCodeKey {
		entry := &e.cfg.CommandCodeKey[i]
		if apiKey != "" && strings.EqualFold(strings.TrimSpace(entry.APIKey), apiKey) {
			if baseURL == "" || strings.EqualFold(strings.TrimSpace(entry.BaseURL), baseURL) {
				return entry
			}
		}
	}
	return nil
}

// commandCodeEntryMatchesAuth reports whether a config entry still describes the
// credential carried by the auth. Comparison is case-insensitive and ignores
// surrounding whitespace, matching the credential matching used elsewhere for
// API-key families. An attribute the auth does not carry is not compared, so a
// partially-populated auth still resolves.
func commandCodeEntryMatchesAuth(entry *config.CommandCodeKey, auth *cliproxyauth.Auth) bool {
	if entry == nil || auth == nil {
		return false
	}
	attr := func(name string) string {
		if auth.Attributes == nil {
			return ""
		}
		return strings.TrimSpace(auth.Attributes[name])
	}
	if key := attr("api_key"); key != "" && !strings.EqualFold(strings.TrimSpace(entry.APIKey), key) {
		return false
	}
	if base := attr("base_url"); base != "" && !strings.EqualFold(strings.TrimSpace(entry.BaseURL), base) {
		return false
	}
	return true
}

// commandCodeAuthID returns the stable identity used to derive the session UUID.
func commandCodeAuthID(auth *cliproxyauth.Auth) string {
	if auth == nil {
		return ""
	}
	if id := strings.TrimSpace(auth.ID); id != "" {
		return id
	}
	return strings.TrimSpace(auth.Label)
}

// commandCodeEndpointURL builds the /alpha/generate URL for a configured base.
func commandCodeEndpointURL(baseURL string) string {
	return strings.TrimSuffix(strings.TrimSpace(baseURL), "/") + commandCodeCLIPath
}
