package classify

import (
	"encoding/json"
	"strings"
)

// SSEError is the outcome of probing an SSE data: payload for an error
// envelope.
type SSEError struct {
	IsError  bool
	Category Category // valid when IsError is true
	Result   Result   // full result (penalize and retryable) when IsError is true
}

// ClassifySSE probes a single parsed SSE data: payload that is already
// JSON-decoded into a map, and classifies it per the SSE rules:
//   - a 429 status or rate-limit marker yields KEY_RATE_LIMITED, recorded with
//     recordError;
//   - an auth-fatal-shaped envelope, indicated by an embedded 401 or 403
//     status or by an authentication_error, permission_error, or
//     authentication type or code, yields KEY_AUTH_FATAL, recorded with
//     markExhausted;
//   - any other error yields UPSTREAM_OUTAGE.
//
// hasErrorEnvelope must be true: the caller has already confirmed an envelope.
// The payload is the parsed JSON object from the data: line.
func ClassifySSE(payload map[string]any) SSEError {
	hay := strings.ToLower(buildProbeString(payload))
	hayStatus := sseStatus(payload)
	hayType := strings.ToLower(sseTypeCode(payload))

	switch {
	case hayStatus == 401 || hayStatus == 403,
		strings.Contains(hayType, "authentication_error"),
		strings.Contains(hayType, "permission_error"),
		strings.Contains(hayType, "authentication"):
		return SSEError{IsError: true, Category: KeyAuthFatal, Result: Result{Category: KeyAuthFatal, Penalize: true, Retryable: true}}
	case hayStatus == 429,
		strings.Contains(hay, "rate_limit"),
		strings.Contains(hay, "rate limit"),
		strings.Contains(hay, "too many requests"),
		strings.Contains(hay, "overloaded"),
		strings.Contains(hay, "quota"),
		strings.Contains(hay, "insufficient_quota"),
		strings.Contains(hay, "throttl"):
		return SSEError{IsError: true, Category: KeyRateLimited, Result: Result{Category: KeyRateLimited, Penalize: true, Retryable: true}}
	default:
		// Penalize is inert: forward.Do's UpstreamOutage branch handles it before applyConsequences.
		return SSEError{IsError: true, Category: UpstreamOutage, Result: Result{Category: UpstreamOutage, Penalize: false, Retryable: true}}
	}
}

// sseStatus extracts an embedded HTTP status from common SSE envelope shapes:
// {"error":{"status":401}}, {"status":401}, or {"error":{"code":"401"}}.
func sseStatus(payload map[string]any) int {
	if s, ok := payload["status"]; ok {
		return toInt(s)
	}
	if errObj, ok := payload["error"].(map[string]any); ok {
		if s, ok := errObj["status"]; ok {
			return toInt(s)
		}
		if c, ok := errObj["code"].(string); ok {
			return toInt(c)
		}
	}
	return 0
}

// sseTypeCode extracts the type/code string fields for auth-fatal detection.
func sseTypeCode(payload map[string]any) string {
	var sb strings.Builder
	add := func(v any) {
		if s, ok := v.(string); ok {
			sb.WriteString(s)
			sb.WriteByte(' ')
		}
	}
	add(payload["type"])
	if errObj, ok := payload["error"].(map[string]any); ok {
		add(errObj["type"])
		add(errObj["code"])
	}
	return sb.String()
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case string:
		var i int
		for _, r := range n {
			if r < '0' || r > '9' {
				return 0
			}
			i = i*10 + int(r-'0')
		}
		return i
	}
	return 0
}

func hasMarkerIn(hayLower string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(hayLower, m) {
			return true
		}
	}
	return false
}

// ParseSSEPayload parses a single `data:` line payload as JSON. It returns
// the decoded object and whether the object is an error envelope: present
// when the object has a top-level "error" key with any value, or a top-level
// "type" equal to "error". A non-JSON or non-object payload returns
// (nil, false).
func ParseSSEPayload(payload string) (map[string]any, bool) {
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil, false
	}
	var obj map[string]any
	if err := json.Unmarshal([]byte(payload), &obj); err != nil {
		return nil, false
	}
	if obj == nil {
		return nil, false
	}
	// An envelope is present when the object has a top-level "error" key with
	// any value or a top-level type equal to "error".
	if _, hasErr := obj["error"]; hasErr {
		return obj, true
	}
	if t, ok := obj["type"].(string); ok && t == "error" {
		return obj, true
	}
	if s := sseStatus(obj); s >= 400 {
		return obj, true
	}
	return obj, false
}
