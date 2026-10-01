package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testServer() *Server {
	s := &Server{Name: "test", Version: "1.0", Instructions: "be nice"}
	s.AddTool(Tool{
		Name:        "echo",
		Description: "echo text",
		ReadOnly:    true,
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			var a struct {
				Text string `json:"text"`
			}
			if err := Args(args, &a); err != nil {
				return "", err
			}
			if a.Text == "" {
				return "", errors.New("text is empty")
			}
			return a.Text, nil
		},
	})
	s.AddResource(Resource{URI: "test://hello", Name: "hello", MimeType: "text/plain", Read: func(context.Context) (string, error) { return "hi", nil }})
	s.AddResourceTemplate(ResourceTemplate{
		URITemplate: "test://items/{name}", Name: "items", MimeType: "text/plain",
		Match: func(uri string) (func(context.Context) (string, error), bool) {
			name, ok := strings.CutPrefix(uri, "test://items/")
			if !ok || name == "" {
				return nil, false
			}
			return func(context.Context) (string, error) { return "item " + name, nil }, true
		},
	})
	return s
}

func call(t *testing.T, s *Server, msg string) map[string]any {
	t.Helper()
	raw := s.HandleMessage(context.Background(), []byte(msg))
	if raw == nil {
		t.Fatalf("no response to %s", msg)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("response %s is not JSON: %v", raw, err)
	}
	return out
}

func TestInitializeNegotiatesVersion(t *testing.T) {
	s := testServer()
	r := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"c","version":"1"}}}`)
	res := r["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" || res["instructions"] != "be nice" {
		t.Errorf("initialize result %v", res)
	}
	caps := res["capabilities"].(map[string]any)
	if caps["tools"] == nil || caps["resources"] == nil {
		t.Errorf("capabilities %v", caps)
	}
	r = call(t, s, `{"jsonrpc":"2.0","id":2,"method":"initialize","params":{"protocolVersion":"2099-01-01"}}`)
	if v := r["result"].(map[string]any)["protocolVersion"]; v != SupportedVersions[0] {
		t.Errorf("unknown version answered with %v, want %s", v, SupportedVersions[0])
	}
}

func TestToolsAndErrors(t *testing.T) {
	s := testServer()
	r := call(t, s, `{"jsonrpc":"2.0","id":"a","method":"tools/list"}`)
	tools := r["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "echo" || r["id"] != "a" {
		t.Fatalf("tools/list %v", r)
	}
	r = call(t, s, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hello"}}}`)
	res := r["result"].(map[string]any)
	if res["isError"] != false || res["content"].([]any)[0].(map[string]any)["text"] != "hello" {
		t.Errorf("tools/call %v", res)
	}
	// Tool failures and bad arguments come back as isError results.
	for _, args := range []string{`{"text":""}`, `{"txet":"x"}`} {
		r = call(t, s, `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"echo","arguments":`+args+`}}`)
		if r["result"].(map[string]any)["isError"] != true {
			t.Errorf("arguments %s: want isError, got %v", args, r)
		}
	}
	// Unknown tools and methods are protocol errors.
	r = call(t, s, `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope"}}`)
	if r["error"].(map[string]any)["code"].(float64) != CodeInvalidParams {
		t.Errorf("unknown tool %v", r)
	}
	r = call(t, s, `{"jsonrpc":"2.0","id":6,"method":"prompts/list"}`)
	if r["error"].(map[string]any)["code"].(float64) != CodeMethodNotFound {
		t.Errorf("unknown method %v", r)
	}
	r = call(t, s, `{not json`)
	if r["error"].(map[string]any)["code"].(float64) != CodeParseError {
		t.Errorf("parse error %v", r)
	}
	if s.HandleMessage(context.Background(), []byte(`{"jsonrpc":"2.0","method":"notifications/initialized"}`)) != nil {
		t.Error("a notification got a response")
	}
}

func TestResources(t *testing.T) {
	s := testServer()
	r := call(t, s, `{"jsonrpc":"2.0","id":1,"method":"resources/read","params":{"uri":"test://items/42"}}`)
	c := r["result"].(map[string]any)["contents"].([]any)[0].(map[string]any)
	if c["text"] != "item 42" || c["mimeType"] != "text/plain" {
		t.Errorf("template read %v", c)
	}
	r = call(t, s, `{"jsonrpc":"2.0","id":2,"method":"resources/read","params":{"uri":"test://missing"}}`)
	if r["error"].(map[string]any)["code"].(float64) != CodeResourceNotFound {
		t.Errorf("missing resource %v", r)
	}
	r = call(t, s, `{"jsonrpc":"2.0","id":3,"method":"resources/templates/list"}`)
	if len(r["result"].(map[string]any)["resourceTemplates"].([]any)) != 1 {
		t.Errorf("templates %v", r)
	}
}

func TestBatch(t *testing.T) {
	s := testServer()
	raw := s.HandleMessage(context.Background(), []byte(`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","method":"notifications/initialized"},{"jsonrpc":"2.0","id":2,"method":"ping"}]`))
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || len(out) != 2 {
		t.Fatalf("batch response %s", raw)
	}
}

func TestStdio(t *testing.T) {
	s := testServer()
	in := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"echo","arguments":{"text":"a\nb"}}}`,
		"",
	}, "\n")
	var out bytes.Buffer
	if err := s.ServeStdio(context.Background(), strings.NewReader(in), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 response lines, got %q", out.String())
	}
	var r map[string]any
	json.Unmarshal([]byte(lines[1]), &r)
	if text := r["result"].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; text != "a\nb" {
		t.Errorf("multi-line text round trip: %q", text)
	}
}

func TestHTTP(t *testing.T) {
	srv := httptest.NewServer(testServer().HTTPHandler())
	defer srv.Close()
	post := func(body, origin string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, "http://localhost:3000")
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/json" || !strings.Contains(string(body), `"result":{}`) {
		t.Errorf("ping: %d %s", resp.StatusCode, body)
	}
	if resp := post(`{"jsonrpc":"2.0","method":"notifications/initialized"}`, ""); resp.StatusCode != http.StatusAccepted {
		t.Errorf("notification: status %d, want 202", resp.StatusCode)
	}
	if resp := post(`{"jsonrpc":"2.0","id":1,"method":"ping"}`, "https://evil.example"); resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign origin: status %d, want 403", resp.StatusCode)
	}
	if resp, _ := http.Get(srv.URL); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET: status %d, want 405", resp.StatusCode)
	}
}
