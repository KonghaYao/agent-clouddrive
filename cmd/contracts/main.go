// contracts emits the canonical MCP tool schema; never contacts external services.
package main

import (
	"encoding/json"
	"github.com/agentclouddrive/agent-clouddrive/internal/transport"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"os"
)

func main() {
	out := []map[string]any{}
	for _, d := range transport.ToolDefinitions {
		out = append(out, map[string]any{"name": d.Name, "description": d.Description, "inputSchema": transport.ToolSchema(d), "scope": d.Scope})
	}
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	if err := e.Encode(map[string]any{"transport": "Streamable HTTP", "sdk": "github.com/modelcontextprotocol/go-sdk v1.8.0", "protocols": mcp.SupportedProtocolVersions(), "tools": out}); err != nil {
		panic(err)
	}
}
