package cli

import (
	"strings"
	"testing"

	"github.com/rytsh/dbq/internal/config"
	"github.com/rytsh/dbq/internal/database"
)

func TestResolveStdioEndpoint(t *testing.T) {
	cfg := config.MCP{
		Enabled: true,
		Endpoints: []config.Endpoint{
			{Path: "/mcp", Permission: "full", Allow: []string{"local"}, Export: true},
			{Path: "/mcp/reporting", Permission: "read-only", Allow: []string{"prod"}},
		},
		ExportEnabled: true,
	}

	endpoint, err := resolveStdioEndpoint(cfg, "mcp/reporting/")
	if err != nil {
		t.Fatalf("resolve stdio endpoint: %v", err)
	}
	if endpoint.Path != "/mcp/reporting" || endpoint.Permission != database.PermissionReadOnly {
		t.Fatalf("endpoint = %+v, want reporting read-only endpoint", endpoint)
	}
	if len(endpoint.Allow) != 1 || endpoint.Allow[0] != "prod" {
		t.Fatalf("allowlist = %v, want [prod]", endpoint.Allow)
	}
}

func TestResolveStdioEndpointDefault(t *testing.T) {
	endpoint, err := resolveStdioEndpoint(config.MCP{Enabled: true}, "/mcp")
	if err != nil {
		t.Fatalf("resolve default endpoint: %v", err)
	}
	if endpoint.Permission != database.PermissionReadOnly {
		t.Fatalf("permission = %s, want read-only", endpoint.Permission)
	}
}

func TestResolveStdioEndpointErrors(t *testing.T) {
	if _, err := resolveStdioEndpoint(config.MCP{}, "/mcp"); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("disabled error = %v", err)
	}

	cfg := config.MCP{
		Enabled:   true,
		Endpoints: []config.Endpoint{{Path: "/mcp/reporting", Permission: "read-only"}},
	}
	if _, err := resolveStdioEndpoint(cfg, "/mcp"); err == nil || !strings.Contains(err.Error(), "/mcp/reporting") {
		t.Fatalf("missing endpoint error = %v", err)
	}
}
