package handlers

import (
	"context"
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/desensitization"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/interfaces"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	log "github.com/sirupsen/logrus"
)

var desensitizationFailClosedBody = []byte(`{"error":{"message":"desensitization failed","type":"proxy_error"}}`)

func desensitizeMaskPayload(ctx context.Context, metadata map[string]any, requestID, model, requestedModel, format, provider, authKind string, body []byte) ([]byte, error) {
	eng := desensitization.Current()
	if eng == nil || !eng.Config().Enabled || len(body) == 0 {
		return body, nil
	}
	clientKey := helps.APIKeyFromContext(ctx)
	if !eng.ShouldMask(clientKey, provider, authKind) {
		return body, nil
	}
	sid := desensitization.SessionIDFromMetadata(metadata, requestID)
	out, err := eng.MaskJSONBody(sid, model, requestedModel, format, body)
	if err != nil {
		if eng.Config().FailClosed {
			log.Warnf("desensitization mask failed (fail_closed): %v", err)
			return nil, err
		}
		return body, nil
	}
	return out, nil
}

func desensitizationFailClosedError(err error) *interfaces.ErrorMessage {
	if err == nil {
		err = fmt.Errorf("desensitization failed")
	}
	return &interfaces.ErrorMessage{
		StatusCode:     http.StatusBadGateway,
		Error:          err,
		DirectResponse: true,
		Body:           append([]byte(nil), desensitizationFailClosedBody...),
	}
}

func desensitizeRestorePayload(metadata map[string]any, requestID string, body []byte) []byte {
	eng := desensitization.Current()
	if eng == nil || !eng.Config().Enabled || !eng.Config().RestoreEnabled() || len(body) == 0 {
		return body
	}
	sid := desensitization.SessionIDFromMetadata(metadata, requestID)
	return eng.RestoreJSONBody(sid, body)
}

func desensitizeRestoreStreamChunk(metadata map[string]any, requestID string, chunk []byte) []byte {
	eng := desensitization.Current()
	if eng == nil || !eng.Config().Enabled || !eng.Config().RestoreEnabled() || len(chunk) == 0 {
		return chunk
	}
	sid := desensitization.SessionIDFromMetadata(metadata, requestID)
	out := eng.RestoreStreamChunk(sid, requestID, chunk)
	if out == nil {
		return []byte{}
	}
	return out
}

func desensitizeFlushStream(metadata map[string]any, requestID string) []byte {
	eng := desensitization.Current()
	if eng == nil || !eng.Config().Enabled || !eng.Config().RestoreEnabled() {
		return nil
	}
	sid := desensitization.SessionIDFromMetadata(metadata, requestID)
	return eng.FlushStream(sid, requestID)
}

func desensitizationActive() bool {
	eng := desensitization.Current()
	return eng != nil && eng.Config().Enabled
}
