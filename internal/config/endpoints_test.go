package config

import "testing"

func TestResolvedEndpoints(t *testing.T) {
	mcp := MCP{Enabled: true, Endpoints: []Endpoint{
		{Path: "/mcp", Permission: "full", Allow: []string{"local", "prod"}},
		{Path: "/mcp/abc", Permission: "read-only", Allow: []string{"prod"}},
	}}

	got, err := mcp.ResolvedEndpoints()
	if err != nil {
		t.Fatalf("ResolvedEndpoints: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("endpoints = %d, want 2", len(got))
	}
	if got[0].Path != "/mcp" || got[0].Permission != "full" {
		t.Errorf("first endpoint = %+v", got[0])
	}
	if got[1].Path != "/mcp/abc" || got[1].Permission != "read-only" {
		t.Errorf("second endpoint = %+v", got[1])
	}
}

func TestResolvedEndpointsDefault(t *testing.T) {
	got, err := (MCP{Enabled: true}).ResolvedEndpoints()
	if err != nil {
		t.Fatalf("ResolvedEndpoints: %v", err)
	}
	if len(got) != 1 || got[0].Path != "/mcp" || got[0].Permission != "read-only" {
		t.Errorf("default endpoint = %+v", got)
	}
}

func TestResolvedEndpointsRejectsExportWithoutGlobalOptIn(t *testing.T) {
	_, err := (MCP{Enabled: true, Endpoints: []Endpoint{{Path: "/mcp", Permission: "read-only", Export: true}}}).ResolvedEndpoints()
	if err == nil {
		t.Fatal("endpoint export succeeded while export_enabled is false")
	}
}
