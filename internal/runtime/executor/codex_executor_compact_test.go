package executor

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v6/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v6/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestCodexExecutorCompactKeepsOriginalPayload(t *testing.T) {
	cases := []struct {
		name    string
		payload string
	}{
		{
			name:    "missing instructions",
			payload: `{"model":"gpt-5.4","input":"hello"}`,
		},
		{
			name:    "null instructions",
			payload: `{"model":"gpt-5.4","instructions":null,"input":"hello"}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath string
			var gotBody []byte
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				body, _ := io.ReadAll(r.Body)
				gotBody = body
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
			}))
			defer server.Close()

			executor := NewCodexExecutor(&config.Config{})
			auth := &cliproxyauth.Auth{Attributes: map[string]string{
				"base_url": server.URL,
				"api_key":  "test",
			}}

			resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
				Model:   "gpt-5.4",
				Payload: []byte(tc.payload),
			}, cliproxyexecutor.Options{
				SourceFormat: sdktranslator.FromString("openai-response"),
				Alt:          "responses/compact",
				Stream:       false,
			})
			if err != nil {
				t.Fatalf("Execute error: %v", err)
			}
			if gotPath != "/responses/compact" {
				t.Fatalf("path = %q, want %q", gotPath, "/responses/compact")
			}
			if tc.name == "missing instructions" && gjson.GetBytes(gotBody, "instructions").Exists() {
				t.Fatalf("instructions should not be injected into compact request, got %s", string(gotBody))
			}
			if tc.name == "null instructions" && gjson.GetBytes(gotBody, "instructions").Type != gjson.Null {
				t.Fatalf("instructions type = %v, want null", gjson.GetBytes(gotBody, "instructions").Type)
			}
			if gjson.GetBytes(gotBody, "tools.0.type").String() == "image_generation" {
				t.Fatalf("image_generation tool should not be injected into compact passthrough body: %s", string(gotBody))
			}
			hasReasoningInclude := false
			gjson.GetBytes(gotBody, "include").ForEach(func(_, v gjson.Result) bool {
				if v.String() == "reasoning.encrypted_content" {
					hasReasoningInclude = true
					return false
				}
				return true
			})
			if !hasReasoningInclude {
				t.Fatalf("compact request should include reasoning.encrypted_content, got %s", string(gotBody))
			}
			if string(resp.Payload) != `{"id":"resp_1","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}` {
				t.Fatalf("payload = %s", string(resp.Payload))
			}
		})
	}
}

func TestCodexExecutorCompactRetriesCompatibilityForUnsupportedContextManagement(t *testing.T) {
	requestCount := 0
	var firstBody []byte
	var secondBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		requestCount++
		switch requestCount {
		case 1:
			firstBody = body
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported parameter: context_management","type":"invalid_request_error"}}`))
		case 2:
			secondBody = body
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp_retry","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}`))
		default:
			t.Fatalf("unexpected extra request %d", requestCount)
		}
	}))
	defer server.Close()

	executor := NewCodexExecutor(&config.Config{})
	auth := &cliproxyauth.Auth{Attributes: map[string]string{
		"base_url": server.URL,
		"api_key":  "test",
	}}

	resp, err := executor.Execute(context.Background(), auth, cliproxyexecutor.Request{
		Model:   "gpt-5.4",
		Payload: []byte(`{"model":"gpt-5.4","context_management":[{"type":"compaction","compact_threshold":12000}],"input":"hello"}`),
	}, cliproxyexecutor.Options{
		SourceFormat: sdktranslator.FromString("openai-response"),
		Alt:          "responses/compact",
		Stream:       false,
	})
	if err != nil {
		t.Fatalf("Execute error: %v", err)
	}
	if requestCount != 2 {
		t.Fatalf("requestCount = %d, want 2", requestCount)
	}
	if !gjson.GetBytes(firstBody, "context_management").Exists() {
		t.Fatalf("first request should preserve context_management: %s", string(firstBody))
	}
	if gjson.GetBytes(firstBody, "instructions").Exists() {
		t.Fatalf("first request should not inject instructions: %s", string(firstBody))
	}
	hasReasoningInclude := func(body []byte) bool {
		found := false
		gjson.GetBytes(body, "include").ForEach(func(_, v gjson.Result) bool {
			if v.String() == "reasoning.encrypted_content" {
				found = true
				return false
			}
			return true
		})
		return found
	}
	if !hasReasoningInclude(firstBody) {
		t.Fatalf("first request should include reasoning.encrypted_content: %s", string(firstBody))
	}
	if gjson.GetBytes(secondBody, "context_management").Exists() {
		t.Fatalf("second request should remove context_management: %s", string(secondBody))
	}
	if gjson.GetBytes(secondBody, "instructions").String() != "" {
		t.Fatalf("second request should inject empty instructions during compatibility retry: %s", string(secondBody))
	}
	if gjson.GetBytes(secondBody, "tools.0.type").String() != "image_generation" {
		t.Fatalf("second request should inject image_generation tool during compatibility retry: %s", string(secondBody))
	}
	if string(resp.Payload) != `{"id":"resp_retry","object":"response.compaction","usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}` {
		t.Fatalf("payload = %s", string(resp.Payload))
	}
}
