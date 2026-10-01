// Package mcp is a small, dependency-free implementation of the server side
// of the Model Context Protocol: JSON-RPC 2.0 with initialize, ping, tools
// and resources, over stdio and streamable HTTP.
//
// It implements only what simo serves. Tool handlers are plain functions of
// JSON arguments to text, so moving to the official Go SDK later only
// changes this package.
package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// SupportedVersions are the protocol versions this server speaks, newest
// first. initialize echoes the client's version when it is listed and
// otherwise answers with the newest.
var SupportedVersions = []string{"2025-11-25", "2025-06-18", "2025-03-26", "2024-11-05"}

// JSON-RPC and MCP error codes.
const (
	CodeParseError       = -32700
	CodeInvalidRequest   = -32600
	CodeMethodNotFound   = -32601
	CodeInvalidParams    = -32602
	CodeInternalError    = -32603
	CodeResourceNotFound = -32002
)

// Tool is a callable tool.
type Tool struct {
	Name        string
	Title       string
	Description string
	InputSchema map[string]any // JSON Schema of the arguments object
	ReadOnly    bool
	// Handler gets the raw arguments object. A returned error becomes a
	// tool result with isError set, so the model can read it and retry.
	Handler func(ctx context.Context, args json.RawMessage) (string, error)
}

// Resource is a readable resource with a fixed URI.
type Resource struct {
	URI         string
	Name        string
	Description string
	MimeType    string
	Read        func(ctx context.Context) (string, error)
}

// ResourceTemplate describes resources by URI template. Match resolves a
// concrete URI to a reader, or returns false.
type ResourceTemplate struct {
	URITemplate string
	Name        string
	Description string
	MimeType    string
	Match       func(uri string) (func(ctx context.Context) (string, error), bool)
}

// Server dispatches MCP requests to tools and resources. Register
// everything before serving.
type Server struct {
	Name         string
	Version      string
	Instructions string

	tools     []Tool
	resources []Resource
	templates []ResourceTemplate
	// ListResources, if set, adds resources discovered at list time (for
	// example worlds created while the server runs).
	ListResources func() []Resource
}

// AddTool registers a tool.
func (s *Server) AddTool(t Tool) {
	if t.InputSchema == nil {
		t.InputSchema = map[string]any{"type": "object"}
	}
	s.tools = append(s.tools, t)
}

// AddResource registers a resource.
func (s *Server) AddResource(r Resource) { s.resources = append(s.resources, r) }

// AddResourceTemplate registers a resource template.
func (s *Server) AddResourceTemplate(t ResourceTemplate) { s.templates = append(s.templates, t) }

// Tools returns the registered tool names.
func (s *Server) Tools() []string {
	names := make([]string, len(s.tools))
	for i, t := range s.tools {
		names[i] = t.Name
	}
	return names
}

type message struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// HandleMessage processes one raw JSON-RPC message (or a batch array) and
// returns the raw response, or nil when nothing should be sent back
// (notifications and responses).
func (s *Server) HandleMessage(ctx context.Context, raw []byte) []byte {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil
	}
	if raw[0] == '[' {
		var batch []json.RawMessage
		if err := json.Unmarshal(raw, &batch); err != nil || len(batch) == 0 {
			return mustJSON(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{CodeInvalidRequest, "invalid batch"}})
		}
		var out []json.RawMessage
		for _, m := range batch {
			if r := s.HandleMessage(ctx, m); r != nil {
				out = append(out, r)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return mustJSON(out)
	}
	var m message
	if err := json.Unmarshal(raw, &m); err != nil {
		return mustJSON(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{CodeParseError, "parse error: " + err.Error()}})
	}
	if m.Method == "" {
		return nil // a response to a request we never send
	}
	isNotification := len(m.ID) == 0 || string(m.ID) == "null"
	result, rerr := s.dispatch(ctx, m.Method, m.Params)
	if isNotification {
		return nil
	}
	resp := response{JSONRPC: "2.0", ID: m.ID}
	if rerr != nil {
		resp.Error = rerr
	} else {
		resp.Result = result
	}
	return mustJSON(resp)
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		b, _ = json.Marshal(response{JSONRPC: "2.0", ID: json.RawMessage("null"), Error: &rpcError{CodeInternalError, err.Error()}})
	}
	return b
}

func (s *Server) dispatch(ctx context.Context, method string, params json.RawMessage) (any, *rpcError) {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(params, &p)
		version := SupportedVersions[0]
		if slices.Contains(SupportedVersions, p.ProtocolVersion) {
			version = p.ProtocolVersion
		}
		caps := map[string]any{"tools": map[string]any{"listChanged": false}}
		if len(s.resources) > 0 || len(s.templates) > 0 || s.ListResources != nil {
			caps["resources"] = map[string]any{"listChanged": false, "subscribe": false}
		}
		res := map[string]any{
			"protocolVersion": version,
			"capabilities":    caps,
			"serverInfo":      map[string]any{"name": s.Name, "version": s.Version},
		}
		if s.Instructions != "" {
			res["instructions"] = s.Instructions
		}
		return res, nil
	case "ping":
		return map[string]any{}, nil
	case "tools/list":
		list := make([]map[string]any, 0, len(s.tools))
		for _, t := range s.tools {
			item := map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema}
			ann := map[string]any{"readOnlyHint": t.ReadOnly}
			if t.Title != "" {
				item["title"] = t.Title
				ann["title"] = t.Title
			}
			item["annotations"] = ann
			list = append(list, item)
		}
		return map[string]any{"tools": list}, nil
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &rpcError{CodeInvalidParams, "bad tools/call params: " + err.Error()}
		}
		i := slices.IndexFunc(s.tools, func(t Tool) bool { return t.Name == p.Name })
		if i < 0 {
			return nil, &rpcError{CodeInvalidParams, fmt.Sprintf("unknown tool %q", p.Name)}
		}
		args := p.Arguments
		if len(args) == 0 || string(args) == "null" {
			args = json.RawMessage("{}")
		}
		text, err := s.tools[i].Handler(ctx, args)
		if err != nil {
			return toolResult(err.Error(), true), nil
		}
		return toolResult(text, false), nil
	case "resources/list":
		all := slices.Clone(s.resources)
		if s.ListResources != nil {
			all = append(all, s.ListResources()...)
		}
		list := make([]map[string]any, 0, len(all))
		for _, r := range all {
			list = append(list, map[string]any{"uri": r.URI, "name": r.Name, "description": r.Description, "mimeType": r.MimeType})
		}
		return map[string]any{"resources": list}, nil
	case "resources/templates/list":
		list := make([]map[string]any, 0, len(s.templates))
		for _, t := range s.templates {
			list = append(list, map[string]any{"uriTemplate": t.URITemplate, "name": t.Name, "description": t.Description, "mimeType": t.MimeType})
		}
		return map[string]any{"resourceTemplates": list}, nil
	case "resources/read":
		var p struct {
			URI string `json:"uri"`
		}
		if err := json.Unmarshal(params, &p); err != nil || p.URI == "" {
			return nil, &rpcError{CodeInvalidParams, "resources/read needs a uri"}
		}
		read, mime := s.findResource(p.URI)
		if read == nil {
			return nil, &rpcError{CodeResourceNotFound, "resource not found: " + p.URI}
		}
		text, err := read(ctx)
		if err != nil {
			return nil, &rpcError{CodeInternalError, err.Error()}
		}
		return map[string]any{"contents": []map[string]any{{"uri": p.URI, "mimeType": mime, "text": text}}}, nil
	}
	if strings.HasPrefix(method, "notifications/") {
		return nil, nil
	}
	return nil, &rpcError{CodeMethodNotFound, "method not found: " + method}
}

func (s *Server) findResource(uri string) (func(context.Context) (string, error), string) {
	all := slices.Clone(s.resources)
	if s.ListResources != nil {
		all = append(all, s.ListResources()...)
	}
	for _, r := range all {
		if r.URI == uri {
			return r.Read, r.MimeType
		}
	}
	for _, t := range s.templates {
		if read, ok := t.Match(uri); ok {
			return read, t.MimeType
		}
	}
	return nil, ""
}

func toolResult(text string, isError bool) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
		"isError": isError,
	}
}

// Args decodes tool arguments strictly: unknown fields are errors, so a
// model that misspells an argument hears about it.
func Args(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var se *json.SyntaxError
		if errors.As(err, &se) {
			return fmt.Errorf("arguments are not valid JSON: %v", err)
		}
		return fmt.Errorf("bad arguments: %v", strings.TrimPrefix(err.Error(), "json: "))
	}
	return nil
}
