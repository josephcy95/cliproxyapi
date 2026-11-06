package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func discardStreamChunks(ch <-chan cliproxyexecutor.StreamChunk) {
	if ch == nil {
		return
	}
	go func() {
		for range ch {
		}
	}()
}

type streamBootstrapError struct {
	cause   error
	headers http.Header
}

type streamBootstrapStatusError struct {
	status int
	body   []byte
}

func (e streamBootstrapStatusError) Error() string {
	return string(e.body)
}

func (e streamBootstrapStatusError) StatusCode() int {
	return e.status
}

func streamBootstrapPayloadError(payload []byte) error {
	candidates := [][]byte{bytes.TrimSpace(payload)}
	for _, line := range bytes.Split(payload, []byte{'\n'}) {
		if data, ok := bytes.CutPrefix(bytes.TrimSpace(line), []byte("data:")); ok {
			candidates = append(candidates, bytes.TrimSpace(data))
		}
	}
	for _, candidate := range candidates {
		if len(candidate) == 0 {
			continue
		}
		errorType := strings.ToLower(strings.TrimSpace(gjson.GetBytes(candidate, "error.type").String()))
		errorCode := strings.ToLower(strings.TrimSpace(gjson.GetBytes(candidate, "error.code").String()))
		if errorType == "service_unavailable_error" && errorCode == "server_is_overloaded" {
			return streamBootstrapStatusError{status: http.StatusServiceUnavailable, body: bytes.Clone(candidate)}
		}
	}
	return nil
}

func cloneHTTPHeader(headers http.Header) http.Header {
	if headers == nil {
		return nil
	}
	return headers.Clone()
}

func newStreamBootstrapError(err error, headers http.Header) error {
	if err == nil {
		return nil
	}
	return &streamBootstrapError{
		cause:   err,
		headers: cloneHTTPHeader(headers),
	}
}

func (e *streamBootstrapError) Error() string {
	if e == nil || e.cause == nil {
		return ""
	}
	return e.cause.Error()
}

func (e *streamBootstrapError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.cause
}

func (e *streamBootstrapError) Headers() http.Header {
	if e == nil {
		return nil
	}
	return cloneHTTPHeader(e.headers)
}

func streamErrorResult(headers http.Header, err error) *cliproxyexecutor.StreamResult {
	ch := make(chan cliproxyexecutor.StreamChunk, 1)
	ch <- cliproxyexecutor.StreamChunk{Err: err}
	close(ch)
	return &cliproxyexecutor.StreamResult{
		Headers: cloneHTTPHeader(headers),
		Chunks:  ch,
	}
}

func validateStreamResult(result *cliproxyexecutor.StreamResult, err error) (*cliproxyexecutor.StreamResult, error) {
	if err != nil {
		return result, err
	}
	if result == nil || result.Chunks == nil {
		return result, &Error{Code: "empty_stream", Message: "upstream stream has no source", Retryable: true}
	}
	return result, nil
}

func readStreamBootstrap(ctx context.Context, ch <-chan cliproxyexecutor.StreamChunk) ([]cliproxyexecutor.StreamChunk, bool, error) {
	if ch == nil {
		return nil, true, nil
	}
	buffered := make([]cliproxyexecutor.StreamChunk, 0, 1)
	for {
		var (
			chunk cliproxyexecutor.StreamChunk
			ok    bool
		)
		if ctx != nil {
			// Prefer a chunk that is already available so an upstream error delivered together
			// with a context cancellation is not discarded.
			select {
			case chunk, ok = <-ch:
			default:
				select {
				case <-ctx.Done():
					return nil, false, ctx.Err()
				case chunk, ok = <-ch:
				}
			}
		} else {
			chunk, ok = <-ch
		}
		if !ok {
			return buffered, true, nil
		}
		if chunk.Err != nil {
			return nil, false, chunk.Err
		}
		if errPayload := streamBootstrapPayloadError(chunk.Payload); errPayload != nil {
			return nil, false, errPayload
		}
		buffered = append(buffered, chunk)
		if len(chunk.Payload) > 0 {
			return buffered, false, nil
		}
	}
}

// recordCanceledUpstreamCooldown keeps an upstream-confirmed cooldown (an error carrying
// RetryAfter) when the request context was canceled while the executor was returning, for
// example because the downstream connection was closed in response to the same upstream error.
// Errors without RetryAfter, such as plain client cancellations, are not recorded.
func (m *Manager) recordCanceledUpstreamCooldown(ctx context.Context, auth *Auth, provider, resultModel, routeModel string, opts cliproxyexecutor.Options, err error, ephemeral bool, executionSeq uint64) {
	retryAfter := retryAfterFromError(err)
	if m == nil || auth == nil || retryAfter == nil {
		return
	}
	action, okAction := matchRequestScopedErrorAction(auth, err, m.runtimeConfigSnapshot())
	result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: resultErrorFromError(err), Options: opts, RetryAfter: retryAfter, AccessToken: authAccessTokenForRequest(ctx, auth, opts.Headers), CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
	result.CredentialScope = isCredentialScopedError(err)
	applyRequestScopedActionToResult(action, okAction, &result)
	m.recordExecutionResult(context.WithoutCancel(ctx), result, auth, ephemeral)
}

func (m *Manager) wrapStreamResult(ctx context.Context, auth *Auth, provider, resultModel, routeModel string, headers http.Header, buffered []cliproxyexecutor.StreamChunk, remaining <-chan cliproxyexecutor.StreamChunk, aliasResult OAuthModelAliasResult, ephemeralResult bool, opts cliproxyexecutor.Options, executionSeq uint64) *cliproxyexecutor.StreamResult {
	ctx = WithCodexClientRequest(ctx, isOpenAIImageExecutionOptions(opts) || IsCodexClientRequest(ctx, opts.Headers))
	accessToken := authAccessTokenForRequest(ctx, auth, opts.Headers)
	out := make(chan cliproxyexecutor.StreamChunk)
	go func() {
		defer close(out)
		defer m.releaseCodexAdaptiveLease(opts)
		var failed bool
		var sawCompleted bool
		var sawIncomplete bool
		forward := true
		var rewriter *StreamRewriter
		if aliasResult.ForceMapping && strings.TrimSpace(aliasResult.OriginalAlias) != "" {
			rewriter = NewStreamRewriter(StreamRewriteOptions{RewriteModel: aliasResult.OriginalAlias})
		}
		emit := func(chunk cliproxyexecutor.StreamChunk) bool {
			if chunk.Err != nil && !failed {
				failed = true
				rerr := resultErrorFromError(chunk.Err)
				action, okAction := matchRequestScopedErrorAction(auth, chunk.Err, m.runtimeConfigSnapshot())
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: opts, AccessToken: accessToken, CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
				result.RetryAfter = retryAfterFromError(chunk.Err)
				result.CredentialScope = isCredentialScopedError(chunk.Err)
				applyRequestScopedActionToResult(action, okAction, &result)
				m.recordExecutionResult(ctx, result, auth, ephemeralResult)
			}
			if !forward {
				return false
			}
			if chunk.Err != nil {
				if ctx == nil {
					out <- chunk
					return true
				}
				select {
				case <-ctx.Done():
					forward = false
					return false
				case out <- chunk:
					return true
				}
			}
			if chunk.Incomplete {
				sawIncomplete = true
			}
			if len(chunk.Payload) == 0 {
				return true
			}
			completedChunk, incompleteChunk := streamPayloadCompletionFlags(chunk.Payload)
			if completedChunk {
				sawCompleted = true
			}
			if incompleteChunk {
				sawIncomplete = true
			}
			payload := rewriteForceMappedStreamChunk(rewriter, chunk.Payload)
			if len(payload) == 0 {
				return true
			}
			chunk.Payload = payload
			if ctx == nil {
				out <- chunk
				return true
			}
			select {
			case <-ctx.Done():
				forward = false
				return false
			case out <- chunk:
				return true
			}
		}
		for _, chunk := range buffered {
			if ok := emit(chunk); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		for chunk := range remaining {
			if ok := emit(chunk); !ok {
				discardStreamChunks(remaining)
				return
			}
		}
		if tail := finishForceMappedStreamChunks(rewriter); len(tail) > 0 {
			tailChunk := cliproxyexecutor.StreamChunk{Payload: tail}
			if !emit(tailChunk) {
				return
			}
		}
		if !failed && (ephemeralResult || claudeOAuthRequestCancellation(ctx, auth, nil) == nil) {
			m.recordExecutionResult(ctx, Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: true, CompletedResponse: sawCompleted && !sawIncomplete, Options: opts, AccessToken: accessToken, CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}, auth, ephemeralResult)
		}
	}()
	return &cliproxyexecutor.StreamResult{Headers: headers, Chunks: out}
}

func streamPayloadCompletionFlags(payload []byte) (completed, incomplete bool) {
	if len(payload) == 0 {
		return false, false
	}
	for _, rawLine := range bytes.Split(payload, []byte("\n")) {
		line := bytes.TrimSpace(rawLine)
		if len(line) == 0 || bytes.HasPrefix(line, []byte(":")) || bytes.HasPrefix(line, []byte("event:")) || bytes.HasPrefix(line, []byte("id:")) || bytes.HasPrefix(line, []byte("retry:")) {
			continue
		}
		data := line
		if bytes.HasPrefix(line, []byte("data:")) {
			data = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		}
		if bytes.Equal(data, []byte("[DONE]")) {
			continue
		}
		if len(data) < 2 || data[0] != '{' || data[len(data)-1] != '}' {
			continue
		}
		comp, incomp := jsonObjectCompletionFlags(data)
		if comp {
			completed = true
		}
		if incomp {
			incomplete = true
		}
	}
	return completed, incomplete
}

func jsonObjectCompletionFlags(rawJSON []byte) (completed, incomplete bool) {
	var obj map[string]any
	if err := json.Unmarshal(rawJSON, &obj); err != nil || len(obj) == 0 {
		return false, false
	}
	if obj["error"] != nil {
		return false, true
	}
	eventType, _ := obj["type"].(string)
	eventType = strings.TrimSpace(eventType)
	status, _ := obj["status"].(string)
	status = strings.ToLower(strings.TrimSpace(status))
	if respObj, ok := obj["response"].(map[string]any); ok {
		if respObj["error"] != nil || respObj["incomplete_details"] != nil {
			incomplete = true
		}
		if respStatus, _ := respObj["status"].(string); strings.TrimSpace(respStatus) != "" {
			status = strings.ToLower(strings.TrimSpace(respStatus))
		}
	}
	switch status {
	case "incomplete", "failed", "cancelled", "canceled":
		return false, true
	}
	switch eventType {
	case "response.incomplete", "response.failed", "response.cancelled", "response.canceled", "error":
		return false, true
	case "response.completed", "response.done", "message_stop":
		if status == "" || status == "completed" {
			return true, incomplete
		}
		return false, true
	case "message_delta":
		if deltaObj, ok := obj["delta"].(map[string]any); ok {
			if sr, okSR := deltaObj["stop_reason"].(string); okSR {
				switch strings.ToLower(strings.TrimSpace(sr)) {
				case "max_tokens", "StopSequenceMaxTokens":
					incomplete = true
				case "end_turn", "stop_sequence", "tool_use":
					completed = true
				}
			}
		}
	}
	if choices, ok := obj["choices"].([]any); ok {
		for _, item := range choices {
			choice, okChoice := item.(map[string]any)
			if !okChoice {
				continue
			}
			fr, okFR := choice["finish_reason"].(string)
			if !okFR {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(fr)) {
			case "length", "content_filter":
				incomplete = true
			case "stop", "tool_calls", "function_call", "end_turn":
				completed = true
			}
		}
	}
	if candidates, ok := obj["candidates"].([]any); ok {
		for _, item := range candidates {
			cand, okCand := item.(map[string]any)
			if !okCand {
				continue
			}
			fr, okFR := cand["finishReason"].(string)
			if !okFR {
				continue
			}
			switch strings.ToUpper(strings.TrimSpace(fr)) {
			case "MAX_TOKENS", "SAFETY", "RECITATION", "OTHER":
				incomplete = true
			case "STOP":
				completed = true
			}
		}
	}
	return completed, incomplete
}

func isCompletedExecutionPayload(payload []byte) bool {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) < 2 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return false
	}
	var obj map[string]any
	if err := json.Unmarshal(trimmed, &obj); err != nil || len(obj) == 0 {
		return false
	}
	if obj["error"] != nil || obj["incomplete_details"] != nil {
		return false
	}
	comp, incomp := jsonObjectCompletionFlags(trimmed)
	if incomp {
		return false
	}
	if comp {
		return true
	}
	status, _ := obj["status"].(string)
	status = strings.ToLower(strings.TrimSpace(status))
	if respObj, ok := obj["response"].(map[string]any); ok {
		if respObj["error"] != nil || respObj["incomplete_details"] != nil {
			return false
		}
		if respStatus, _ := respObj["status"].(string); strings.TrimSpace(respStatus) != "" {
			status = strings.ToLower(strings.TrimSpace(respStatus))
		}
	}
	switch status {
	case "incomplete", "failed", "cancelled", "canceled", "in_progress", "queued":
		return false
	case "completed":
		return true
	}
	if objType, _ := obj["type"].(string); strings.TrimSpace(objType) == "message" {
		if stopReason, ok := obj["stop_reason"].(string); ok && strings.TrimSpace(stopReason) != "" {
			return true
		}
	}
	if output, ok := obj["output"].([]any); ok && len(output) > 0 && status == "" {
		return true
	}
	return false
}

func (m *Manager) replaceHomeExecutionLifecycleAuth(lifecycle cliproxyexecutor.ExecutionLifecycle, auth *Auth) {
	selection, ok := lifecycle.(*HomeDispatchSelection)
	if !ok || selection == nil {
		return
	}
	m.replaceHomeSelectionAuth(selection, auth)
}

func (m *Manager) executeStreamWithModelPool(ctx context.Context, executor ProviderExecutor, auth *Auth, provider string, req cliproxyexecutor.Request, opts cliproxyexecutor.Options, routeModel, executionModel string, execModels []string, pooled bool, aliasResult OAuthModelAliasResult, routing *apiKeyModelRoutingSnapshot, allowRetry bool, ephemeralResult bool, unauthorizedRefreshTried map[string]struct{}) (*cliproxyexecutor.StreamResult, error) {
	if executor == nil {
		return nil, &Error{Code: "executor_not_found", Message: "executor not registered"}
	}
	executor = executorForAuth(executor, auth)
	ctx = contextWithRequestedModelAlias(ctx, opts, routeModel)
	var lastErr error
	var upstreamErr error
	didRefreshOnUnauthorized := false
	if auth != nil && unauthorizedRefreshTried != nil {
		_, didRefreshOnUnauthorized = unauthorizedRefreshTried[auth.ID]
	}
	for idx, execModel := range execModels {
		ctx = newUpstreamAttemptContext(ctx)
		resultModel := m.stateModelForExecution(auth, routeModel, execModel, pooled)
		execReq := req
		execReq.Model = execModel
		if executionModel != "" {
			execReq.Model = executionModel
		}
		execOpts := opts
		var errIntercept error
		execReq, execOpts, errIntercept = applyRequestAfterAuthInterceptor(ctx, executor, auth, provider, execReq, execOpts, requestedModelAliasFromOptions(execOpts, routeModel))
		if errIntercept != nil {
			m.releaseCodexAdaptiveLease(opts)
			return nil, errIntercept
		}
		execReq = attachResolvedExecutionModelInfo(routing, execReq, auth, routeModel, execModel, executionModel != "")
		if executionModel == "" {
			if selection, ok := opts.ExecutionLifecycle.(*HomeDispatchSelection); ok && selection != nil {
				execReq = attachResolvedHomeModelInfo(execReq, auth, routeModel, selection.modelInfo, selection.configurationUpdateSupport)
			}
		}
		if errCtx := ctx.Err(); errCtx != nil {
			m.releaseCodexAdaptiveLease(opts)
			return nil, errCtx
		}
		ctx = syncMetadataSessionToContext(ctx, execOpts.Metadata)
		entry := logEntryWithRequestID(ctx)
		executionSeq := m.nextExecutionSeq()
		startStream := time.Now()
		streamResult, errStream := executor.ExecuteStream(ctx, auth, execReq, execOpts)
		errStream = markUpstreamExecutionAttemptFromContext(ctx, errStream)
		if hasUpstreamExecutionAttempt(errStream) {
			upstreamErr = errStream
		}
		if errStream != nil {
			warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startStream), errStream)
		}
		if errStream != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				m.recordCanceledUpstreamCooldown(ctx, auth, provider, resultModel, routeModel, execOpts, errStream, ephemeralResult, executionSeq)
				m.releaseCodexAdaptiveLease(execOpts)
				return nil, errCtx
			}
			if allowRetry {
				alreadyTried := didRefreshOnUnauthorized
				willAttemptHomeRefresh := ephemeralResult && !alreadyTried && auth != nil && auth.AuthKind() == AuthKindOAuth && isUnauthorizedError(errStream)
				refreshCtx := newUpstreamAttemptContext(ctx)
				refreshed, okRefresh, errRefresh := m.tryRefreshExecutionAuthAfterUnauthorized(refreshCtx, executor, auth, errStream, alreadyTried, ephemeralResult, execOpts.Headers)
				if willAttemptHomeRefresh {
					didRefreshOnUnauthorized = true
					if unauthorizedRefreshTried != nil {
						unauthorizedRefreshTried[auth.ID] = struct{}{}
					}
				}
				if errRefresh != nil {
					errStream = markUpstreamExecutionAttempt(errRefresh)
					upstreamErr = errStream
				} else if okRefresh {
					auth = refreshed
					m.replaceHomeExecutionLifecycleAuth(execOpts.ExecutionLifecycle, auth)
					publishSelectedAuthMetadata(execOpts.Metadata, auth)
					didRefreshOnUnauthorized = true
					ctx = newUpstreamAttemptContext(ctx)
					ctx = syncMetadataSessionToContext(ctx, execOpts.Metadata)
					executionSeq = m.nextExecutionSeq()
					startRetry := time.Now()
					streamResult, errStream = executor.ExecuteStream(ctx, auth, execReq, execOpts)
					errStream = markUpstreamExecutionAttemptFromContext(ctx, errStream)
					if hasUpstreamExecutionAttempt(errStream) {
						upstreamErr = errStream
					}
					if errStream != nil {
						warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startRetry), errStream)
					}
					if errStream != nil {
						if errCtx := ctx.Err(); errCtx != nil {
							m.releaseCodexAdaptiveLease(opts)
							m.recordCanceledUpstreamCooldown(ctx, auth, provider, resultModel, routeModel, execOpts, errStream, ephemeralResult, executionSeq)
							return nil, errCtx
						}
					}
				}
			}
		}
		if !ephemeralResult {
			if errCancel := claudeOAuthRequestCancellation(ctx, auth, errStream); errCancel != nil {
				m.releaseCodexAdaptiveLease(opts)
				return nil, errCancel
			}
		}
		streamResult, errStream = validateStreamResult(streamResult, errStream)
		errStream = markUpstreamExecutionAttemptFromContext(ctx, errStream)
		if errStream != nil {
			rerr := resultErrorFromError(errStream)
			action, okAction := matchRequestScopedErrorAction(auth, errStream, m.runtimeConfigSnapshot())
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts, AccessToken: authAccessTokenForRequest(ctx, auth, execOpts.Headers), CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
			result.RetryAfter = retryAfterFromError(errStream)
			if isCredentialScopedError(errStream) {
				result.CredentialScope = true
			}
			applyRequestScopedActionToResult(action, okAction, &result)
			m.recordExecutionResult(ctx, result, auth, ephemeralResult)
			errStream = annotateTrustedRetryAfterByHint(errStream)
			syncPreferredUpstreamError(&upstreamErr, errStream)
			if okAction {
				if isRequestScopedStop(action, okAction) {
					return nil, wrapRequestStopError(errStream)
				}
				lastErr = errStream
				if result.CredentialScope {
					return nil, errStream
				}
				continue
			}
			if isRequestInvalidError(errStream) || cliproxyexecutor.IsUpstreamWebsocketTurnError(errStream) {
				return nil, errStream
			}
			lastErr = errStream
			if result.CredentialScope {
				return nil, errStream
			}
			continue
		}

		buffered, closed, bootstrapErr := readStreamBootstrap(ctx, streamResult.Chunks)
		bootstrapErr = markUpstreamExecutionAttemptFromContext(ctx, bootstrapErr)
		if hasUpstreamExecutionAttempt(bootstrapErr) {
			upstreamErr = newStreamBootstrapError(bootstrapErr, streamResult.Headers)
		}
		if bootstrapErr != nil {
			if errCtx := ctx.Err(); errCtx != nil {
				m.recordCanceledUpstreamCooldown(ctx, auth, provider, resultModel, routeModel, execOpts, bootstrapErr, ephemeralResult, executionSeq)
				discardStreamChunks(streamResult.Chunks)
				m.releaseCodexAdaptiveLease(opts)
				return nil, errCtx
			}
			if allowRetry {
				alreadyTried := didRefreshOnUnauthorized
				willAttemptHomeRefresh := ephemeralResult && !alreadyTried && auth != nil && auth.AuthKind() == AuthKindOAuth && isUnauthorizedError(bootstrapErr)
				refreshCtx := newUpstreamAttemptContext(ctx)
				refreshed, okRefresh, errRefresh := m.tryRefreshExecutionAuthAfterUnauthorized(refreshCtx, executor, auth, bootstrapErr, alreadyTried, ephemeralResult, execOpts.Headers)
				if willAttemptHomeRefresh {
					didRefreshOnUnauthorized = true
					if unauthorizedRefreshTried != nil {
						unauthorizedRefreshTried[auth.ID] = struct{}{}
					}
				}
				if errRefresh != nil {
					discardStreamChunks(streamResult.Chunks)
					bootstrapErr = markUpstreamExecutionAttempt(errRefresh)
					upstreamErr = bootstrapErr
					streamResult = &cliproxyexecutor.StreamResult{}
				} else if okRefresh {
					discardStreamChunks(streamResult.Chunks)
					auth = refreshed
					m.replaceHomeExecutionLifecycleAuth(execOpts.ExecutionLifecycle, auth)
					publishSelectedAuthMetadata(execOpts.Metadata, auth)
					didRefreshOnUnauthorized = true
					ctx = newUpstreamAttemptContext(ctx)
					executionSeq = m.nextExecutionSeq()
					startRetry := time.Now()
					retryStream, retryErr := executor.ExecuteStream(ctx, auth, execReq, execOpts)
					retryStream, retryErr = validateStreamResult(retryStream, retryErr)
					retryErr = markUpstreamExecutionAttemptFromContext(ctx, retryErr)
					if retryErr != nil {
						warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startRetry), retryErr)
					}
					if retryErr != nil {
						if errCtx := ctx.Err(); errCtx != nil {
							m.releaseCodexAdaptiveLease(opts)
							m.recordCanceledUpstreamCooldown(ctx, auth, provider, resultModel, routeModel, execOpts, retryErr, ephemeralResult, executionSeq)
							return nil, errCtx
						}
						bootstrapErr = retryErr
						streamResult = &cliproxyexecutor.StreamResult{}
					} else {
						streamResult = retryStream
						buffered, closed, bootstrapErr = readStreamBootstrap(ctx, streamResult.Chunks)
						bootstrapErr = markUpstreamExecutionAttemptFromContext(ctx, bootstrapErr)
					}
				}
			}
		}
		if hasUpstreamExecutionAttempt(bootstrapErr) {
			upstreamErr = newStreamBootstrapError(bootstrapErr, streamResult.Headers)
		}
		if !ephemeralResult {
			if errCancel := claudeOAuthRequestCancellation(ctx, auth, bootstrapErr); errCancel != nil {
				discardStreamChunks(streamResult.Chunks)
				m.releaseCodexAdaptiveLease(opts)
				return nil, errCancel
			}
		}
		if bootstrapErr != nil {
			action, okAction := matchRequestScopedErrorAction(auth, bootstrapErr, m.runtimeConfigSnapshot())
			if okAction {
				rerr := resultErrorFromError(bootstrapErr)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts, AccessToken: authAccessTokenForRequest(ctx, auth, execOpts.Headers), CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				if isCredentialScopedError(bootstrapErr) {
					result.CredentialScope = true
				}
				applyRequestScopedActionToResult(action, okAction, &result)
				m.recordExecutionResult(ctx, result, auth, ephemeralResult)
				bootstrapErr = annotateTrustedRetryAfterByHint(bootstrapErr)
				bootstrapErr = newStreamBootstrapError(bootstrapErr, streamResult.Headers)
				syncPreferredUpstreamError(&upstreamErr, bootstrapErr)
				discardStreamChunks(streamResult.Chunks)
				if isRequestScopedStop(action, okAction) {
					return nil, wrapRequestStopError(bootstrapErr)
				}
				lastErr = bootstrapErr
				if result.CredentialScope {
					return nil, newStreamBootstrapError(bootstrapErr, streamResult.Headers)
				}
				continue
			}
			if isRequestInvalidError(bootstrapErr) || cliproxyexecutor.IsUpstreamWebsocketTurnError(bootstrapErr) {
				rerr := resultErrorFromError(bootstrapErr)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts, AccessToken: authAccessTokenForRequest(ctx, auth, execOpts.Headers), CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				if isCredentialScopedError(bootstrapErr) {
					result.CredentialScope = true
				}
				m.recordExecutionResult(ctx, result, auth, ephemeralResult)
				bootstrapErr = annotateTrustedRetryAfterByHint(bootstrapErr)
				syncPreferredUpstreamError(&upstreamErr, bootstrapErr)
				discardStreamChunks(streamResult.Chunks)
				return nil, bootstrapErr
			}
			if idx < len(execModels)-1 {
				rerr := resultErrorFromError(bootstrapErr)
				result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts, AccessToken: authAccessTokenForRequest(ctx, auth, execOpts.Headers), CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
				result.RetryAfter = retryAfterFromError(bootstrapErr)
				if isCredentialScopedError(bootstrapErr) {
					result.CredentialScope = true
				}
				m.recordExecutionResult(ctx, result, auth, ephemeralResult)
				bootstrapErr = annotateTrustedRetryAfterByHint(bootstrapErr)
				bootstrapErr = newStreamBootstrapError(bootstrapErr, streamResult.Headers)
				syncPreferredUpstreamError(&upstreamErr, bootstrapErr)
				discardStreamChunks(streamResult.Chunks)
				lastErr = bootstrapErr
				if result.CredentialScope {
					return nil, newStreamBootstrapError(bootstrapErr, streamResult.Headers)
				}
				continue
			}
			rerr := resultErrorFromError(bootstrapErr)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: rerr, Options: execOpts, AccessToken: authAccessTokenForRequest(ctx, auth, execOpts.Headers), CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
			result.RetryAfter = retryAfterFromError(bootstrapErr)
			if isCredentialScopedError(bootstrapErr) {
				result.CredentialScope = true
			}
			m.recordExecutionResult(ctx, result, auth, ephemeralResult)
			bootstrapErr = annotateTrustedRetryAfterByHint(bootstrapErr)
			bootstrapErr = newStreamBootstrapError(bootstrapErr, streamResult.Headers)
			syncPreferredUpstreamError(&upstreamErr, bootstrapErr)
			discardStreamChunks(streamResult.Chunks)
			return nil, newStreamBootstrapError(bootstrapErr, streamResult.Headers)
		}

		if closed && len(buffered) == 0 {
			emptyErr := markUpstreamExecutionAttemptFromContext(ctx, &Error{Code: "empty_stream", Message: "upstream stream closed before first payload", Retryable: true})
			currentErr := newStreamBootstrapError(emptyErr, streamResult.Headers)
			if hasUpstreamExecutionAttempt(emptyErr) {
				upstreamErr = currentErr
			}
			warnLogUpstreamFailure(ctx, entry, provider, execModel, auth, time.Since(startStream), emptyErr)
			result := Result{AuthID: auth.ID, Provider: provider, Model: resultModel, RouteModel: routeModel, Success: false, Error: resultErrorFromError(emptyErr), Options: execOpts, AccessToken: authAccessTokenForRequest(ctx, auth, execOpts.Headers), CredentialVersion: auth.CredentialVersion, RegistrationEpoch: auth.RegistrationEpoch, ExecutionSeq: executionSeq}
			m.recordExecutionResult(ctx, result, auth, ephemeralResult)
			if idx < len(execModels)-1 {
				lastErr = emptyErr
				continue
			}
			return nil, newStreamBootstrapError(emptyErr, streamResult.Headers)
		}

		remaining := streamResult.Chunks
		if closed {
			closedCh := make(chan cliproxyexecutor.StreamChunk)
			close(closedCh)
			remaining = closedCh
		}
		attemptAliasResult := resolveAttemptAliasResult(routing, auth, routeModel, execModel, aliasResult)
		return m.wrapStreamResult(ctx, auth.Clone(), provider, resultModel, routeModel, streamResult.Headers, buffered, remaining, attemptAliasResult, ephemeralResult, execOpts, executionSeq), nil
	}
	if lastErr == nil {
		lastErr = &Error{Code: "auth_not_found", Message: "no upstream model available"}
	}
	return nil, preferredExecutionAttemptError(lastErr, upstreamErr)
}
