package plugin

import (
	"encoding/json"
	"strconv"
	"strings"
)

const maxFailureReason = 300

// Body stays empty when the failure was a bare transport error, not a response.
type usageFailureView struct {
	StatusCode int
	ErrorType  string
	Reason     string
	Body       string
}

// Only feeds the reason and the category; the payload itself is stored verbatim.
type upstreamFailure struct {
	Message string
	Type    string
	Code    string
}

func usageFailureDetails(failure UsageFailure) usageFailureView {
	statusCode := validFailureStatus(failure.StatusCode)
	raw := strings.TrimSpace(failure.Body)
	payload, qualifier, structured := parseUpstreamFailure(raw)
	if !structured {
		payload = upstreamFailure{Message: raw}
	}
	reason := formatFailureReason(statusCode, qualifyFailure(payload.Message, qualifier))
	view := usageFailureView{StatusCode: statusCode, ErrorType: payload.errorType(reason), Reason: reason}
	if structured {
		view.Body = raw
	}
	return view
}

func (f upstreamFailure) errorType(reason string) string {
	if f.Code != "" {
		return f.Code
	}
	if f.Type != "" {
		return f.Type
	}
	return inferredFailureType(reason)
}

// A reason may wrap the signature in an HTTP prefix and a qualifier.
func inferredFailureType(reason string) string {
	reason = strings.ToLower(reason)
	switch {
	case strings.Contains(reason, "websocket: close 1006"):
		return "websocket_abnormal_closure"
	case strings.Contains(reason, "context canceled"):
		return "context_canceled"
	default:
		return ""
	}
}

func parseUpstreamFailure(raw string) (payload upstreamFailure, qualifier string, structured bool) {
	if raw == "" {
		return upstreamFailure{}, "", false
	}
	if start := strings.IndexByte(raw, '{'); start > 0 {
		raw = raw[start:]
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var root map[string]any
	if decoder.Decode(&root) != nil || root == nil {
		return upstreamFailure{}, "", false
	}

	var node map[string]any
	for _, path := range [][]string{{"error"}, {"response", "error"}, {"body", "error"}} {
		if node = failureObjectAt(root, path...); node != nil {
			break
		}
	}
	rootNode := false
	message := ""
	if node != nil {
		message = failureString(node["message"])
	} else if value := failureString(root["error"]); value != "" {
		message = value
		node = root
		rootNode = true
	} else if value := failureString(root["message"]); value != "" {
		message = value
		node = root
		rootNode = true
	}
	if node == nil {
		return upstreamFailure{}, "", false
	}

	errorType := failureString(node["type"])
	qualifier = errorType
	if rootNode && strings.EqualFold(errorType, "error") {
		errorType = ""
		qualifier = failureString(node["code"])
	} else if qualifier == "" {
		qualifier = failureString(node["status"])
		if qualifier == "" {
			qualifier = failureString(node["code"])
		}
	}
	payload = upstreamFailure{Message: message, Type: errorType, Code: failureString(node["code"])}
	if payload == (upstreamFailure{}) {
		// No scalars to read; no more usable than plain text.
		return upstreamFailure{}, "", false
	}
	return payload, qualifier, true
}

func validFailureStatus(status int) int {
	if status < 100 || status > 599 {
		return 0
	}
	return status
}

func formatFailureReason(statusCode int, message string) string {
	status := ""
	if statusCode != 0 {
		status = "HTTP " + strconv.Itoa(statusCode)
	}
	switch {
	case message == "":
		return status
	case status == "" || strings.Contains(message, status):
		return truncateFailureReason(message)
	default:
		return truncateFailureReason(status + "：" + message)
	}
}

func failureObjectAt(root map[string]any, path ...string) map[string]any {
	var current any = root
	for _, key := range path {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current = object[key]
	}
	object, _ := current.(map[string]any)
	return object
}

func failureString(value any) string {
	switch value := value.(type) {
	case string:
		return strings.TrimSpace(value)
	case json.Number:
		return value.String()
	default:
		return ""
	}
}

func qualifyFailure(message, code string) string {
	if message == "" || code == "" || strings.Contains(message, code) {
		return message
	}
	return message + "（" + code + "）"
}

func truncateFailureReason(reason string) string {
	reason = strings.TrimSpace(reason)
	runes := []rune(reason)
	if len(runes) <= maxFailureReason {
		return reason
	}
	return strings.TrimSpace(string(runes[:maxFailureReason])) + "…"
}
