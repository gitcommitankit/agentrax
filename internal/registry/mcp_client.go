/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package registry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// DefaultHandshakeTimeout is the HTTP timeout for MCP initialization requests.
	DefaultHandshakeTimeout = 10 * time.Second

	// DefaultMCPProtocolVersion is the Model Context Protocol specification version advertised.
	DefaultMCPProtocolVersion = "2024-11-05"
)

// MCPClient defines the interface for communicating with an agent's MCP endpoint.
type MCPClient interface {
	// Initialize performs the MCP protocol handshake against the given agent endpoint URL
	// (e.g. "http://agent-svc.namespace.svc:8080") and returns the list of advertised tool names.
	Initialize(ctx context.Context, endpoint string) ([]string, error)
}

// httpMCPClient is the default HTTP-based implementation of MCPClient.
type httpMCPClient struct {
	httpClient *http.Client
}

// NewHTTPMCPClient creates a new HTTP-based MCPClient with default timeout settings.
func NewHTTPMCPClient() MCPClient {
	return &httpMCPClient{
		httpClient: &http.Client{
			Timeout: DefaultHandshakeTimeout,
		},
	}
}

// mcpInitializeRequest represents the JSON-RPC 2.0 initialize request payload.
type mcpInitializeRequest struct {
	JSONRPC string              `json:"jsonrpc"`
	ID      int                 `json:"id"`
	Method  string              `json:"method"`
	Params  mcpInitializeParams `json:"params"`
}

type mcpInitializeParams struct {
	ProtocolVersion string                 `json:"protocolVersion"`
	ClientInfo      mcpClientInfo          `json:"clientInfo"`
	Capabilities    map[string]interface{} `json:"capabilities"`
}

type mcpClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// mcpInitializeResponse represents the JSON-RPC 2.0 initialize response payload.
type mcpInitializeResponse struct {
	JSONRPC string               `json:"jsonrpc"`
	ID      int                  `json:"id"`
	Result  *mcpInitializeResult `json:"result,omitempty"`
	Error   *mcpError            `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type mcpInitializeResult struct {
	ProtocolVersion string              `json:"protocolVersion"`
	Capabilities    mcpCapabilities     `json:"capabilities"`
	ServerInfo      *mcpClientInfo      `json:"serverInfo,omitempty"`
	Tools           []mcpToolDefinition `json:"tools,omitempty"`
}

type mcpCapabilities struct {
	Tools *mcpToolsCapability `json:"tools,omitempty"`
}

type mcpToolsCapability struct {
	Available []string `json:"available,omitempty"`
	List      bool     `json:"list,omitempty"`
}

type mcpToolDefinition struct {
	Name string `json:"name"`
}

// Initialize performs an HTTP POST handshake to the agent's MCP endpoint and extracts tool names.
func (c *httpMCPClient) Initialize(ctx context.Context, endpoint string) ([]string, error) {
	url := strings.TrimRight(endpoint, "/")
	// If endpoint doesn't end with a specific path, target /initialize or root
	if !strings.HasSuffix(url, "/initialize") {
		url += "/initialize"
	}

	reqBody := mcpInitializeRequest{
		JSONRPC: "2.0",
		ID:      1,
		Method:  "initialize",
		Params: mcpInitializeParams{
			ProtocolVersion: DefaultMCPProtocolVersion,
			ClientInfo: mcpClientInfo{
				Name:    "agentrax",
				Version: "1.0",
			},
			Capabilities: map[string]interface{}{},
		},
	}

	bodyBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling MCP initialize request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("creating MCP initialize request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("sending MCP initialize to %s: %w", url, err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("MCP initialize returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var initResp mcpInitializeResponse
	if err := json.NewDecoder(resp.Body).Decode(&initResp); err != nil {
		return nil, fmt.Errorf("decoding MCP initialize response: %w", err)
	}

	if initResp.Error != nil {
		return nil, fmt.Errorf("MCP initialize RPC error (code %d): %s", initResp.Error.Code, initResp.Error.Message)
	}

	if initResp.Result == nil {
		return nil, fmt.Errorf("MCP initialize response missing result object")
	}

	// Extract tools: check capabilities.tools.available first, then result.tools list
	var tools []string
	if initResp.Result.Capabilities.Tools != nil && len(initResp.Result.Capabilities.Tools.Available) > 0 {
		tools = append(tools, initResp.Result.Capabilities.Tools.Available...)
	} else if len(initResp.Result.Tools) > 0 {
		for _, t := range initResp.Result.Tools {
			if t.Name != "" {
				tools = append(tools, t.Name)
			}
		}
	}

	return tools, nil
}
