package httpapi

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDsAndStructuredAccessLogs(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	handler := New(nil).WithLogger(logger).WithTraceProject("datatrains-test").Handler()

	request := httptest.NewRequest(http.MethodGet, "/livez", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	requestID := response.Header().Get("X-Request-ID")
	if response.Code != http.StatusOK || !strings.HasPrefix(requestID, "req_") {
		t.Fatalf("response=%d request-id=%q", response.Code, requestID)
	}
	if traceparent := response.Header().Get("Traceparent"); len(traceparent) != 55 || !strings.HasPrefix(traceparent, "00-") {
		t.Fatalf("generated traceparent=%q", traceparent)
	}
	var entry map[string]any
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatalf("access log is not JSON: %v\n%s", err, output.String())
	}
	if entry["msg"] != "http_request" || entry["request_id"] != requestID || entry["path"] != "/livez" || entry["status"] != float64(http.StatusOK) {
		t.Fatalf("unexpected structured access log: %#v", entry)
	}
	if entry["trace_id"] == "" || entry["span_id"] == "" || entry["logging.googleapis.com/trace"] != "projects/datatrains-test/traces/"+entry["trace_id"].(string) {
		t.Fatalf("missing Cloud Trace correlation: %#v", entry)
	}

	invalid := httptest.NewRequest(http.MethodGet, "/livez", nil)
	invalid.Header.Set("X-Request-ID", "unsafe request id\n")
	invalidResponse := httptest.NewRecorder()
	handler.ServeHTTP(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid request ID response=%d, want 400", invalidResponse.Code)
	}
}

func TestTraceContextIsValidatedAndPreserved(t *testing.T) {
	handler := New(nil).Handler()
	const traceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	request := httptest.NewRequest(http.MethodGet, "/livez", nil)
	request.Header.Set("Traceparent", traceparent)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if got := response.Header().Get("Traceparent"); got != traceparent {
		t.Fatalf("traceparent=%q, want %q", got, traceparent)
	}

	cloud, ok := parseCloudTrace("4bf92f3577b34da6a3ce929d0e0e4736/67667974448284343;o=1")
	if !ok || cloud.TraceID != "4bf92f3577b34da6a3ce929d0e0e4736" || cloud.SpanID != "00f067aa0ba902b7" || !cloud.Sampled {
		t.Fatalf("parsed Cloud trace=%#v ok=%v", cloud, ok)
	}
	if _, ok := parseTraceparent("00-00000000000000000000000000000000-0000000000000000-01"); ok {
		t.Fatal("zero trace identifiers must be rejected")
	}
}
