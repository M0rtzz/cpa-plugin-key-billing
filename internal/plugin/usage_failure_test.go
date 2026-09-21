package plugin

import "testing"

func TestUsageFailureDetails(t *testing.T) {
	const closure = "websocket: close 1006 (abnormal closure): unexpected EOF"
	const rateLimit = `{"error":{"message":"rate limited","type":"rate_limit_error","code":"slow_down","meta":{"n":1}},"request_id":"req_42"}`
	const serverError = `{"error":{"message":"boom","type":"server_error"}}`
	for _, test := range []struct {
		name       string
		statusCode int
		body       string
		want       usageFailureView
	}{{
		name: "payload is stored verbatim and its code names the failure", statusCode: 429, body: rateLimit,
		want: usageFailureView{StatusCode: 429, ErrorType: "slow_down", Reason: "HTTP 429：rate limited（rate_limit_error）", Body: rateLimit},
	}, {
		name: "payload without a code falls back to its type", body: serverError,
		want: usageFailureView{ErrorType: "server_error", Reason: "boom（server_error）", Body: serverError},
	}, {
		name: "payload without a category is classified from its reason", body: `{"error":{"message":"` + closure + `"}}`,
		want: usageFailureView{ErrorType: "websocket_abnormal_closure", Reason: closure, Body: `{"error":{"message":"` + closure + `"}}`},
	}, {
		name: "transport error is classified without a body", statusCode: 502, body: closure,
		want: usageFailureView{StatusCode: 502, ErrorType: "websocket_abnormal_closure", Reason: "HTTP 502：" + closure},
	}, {
		name: "cancellation is classified", body: "context canceled",
		want: usageFailureView{ErrorType: "context_canceled", Reason: "context canceled"},
	}, {
		name: "unknown signature stays unclassified", statusCode: 308, body: "redirect failed",
		want: usageFailureView{StatusCode: 308, Reason: "HTTP 308：redirect failed"},
	}, {
		name: "unreadable response survives in the reason", body: `{"error":`,
		want: usageFailureView{Reason: `{"error":`},
	}} {
		t.Run(test.name, func(t *testing.T) {
			if got := usageFailureDetails(UsageFailure{StatusCode: test.statusCode, Body: test.body}); got != test.want {
				t.Errorf("details = %+v, want %+v", got, test.want)
			}
		})
	}
}
