package database

import (
	"strings"
	"testing"
)

func TestIngresTableQueryUsesNativeCatalog(t *testing.T) {
	query, args := tableQuery("ingres", "owner")

	if !strings.Contains(query, "FROM iitables") || strings.Contains(query, "information_schema") {
		t.Errorf("query = %q, want Ingres catalog", query)
	}
	if len(args) != 1 || args[0] != "owner" {
		t.Errorf("args = %v, want [owner]", args)
	}
}

func TestIngresForeignKeysDegradeGracefully(t *testing.T) {
	query, args := foreignKeyQuery("ingres", "", "relation")
	if query != "" || args != nil {
		t.Errorf("query, args = %q, %v; want no foreign-key query", query, args)
	}
}
