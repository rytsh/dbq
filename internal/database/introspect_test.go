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

// TestSQLServerForeignKeysUseSysCatalog guards against the information_schema
// form, whose position_in_unique_constraint column SQL Server does not have.
func TestSQLServerForeignKeysUseSysCatalog(t *testing.T) {
	query, args := foreignKeyQuery("sqlserver", "dbo", "orders")

	if !strings.Contains(query, "sys.foreign_key_columns") || strings.Contains(query, "position_in_unique_constraint") {
		t.Errorf("query = %q, want sys catalog", query)
	}

	if len(args) != 2 || args[0] != "orders" || args[1] != "dbo" {
		t.Errorf("args = %v, want [orders dbo]", args)
	}
}
