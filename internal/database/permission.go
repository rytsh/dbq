package database

import (
	"fmt"
	"strings"
)

// Permission is the access level granted to a caller for a connection.
//
// The levels are ordered: PermissionReadOnly < PermissionSafeWrite < PermissionFull.
type Permission string

const (
	// PermissionReadOnly allows only statements that cannot change data or schema.
	PermissionReadOnly Permission = "read-only"
	// PermissionSafeWrite additionally allows DML whose reach can be shown to be
	// bounded: an INSERT ... VALUES, or an UPDATE/DELETE with an effective
	// WHERE clause on a single table.
	PermissionSafeWrite Permission = "safe-write"
	// PermissionFull allows everything else: DDL, privilege changes, and writes
	// that could affect an unbounded number of rows.
	PermissionFull Permission = "full"
)

// DefaultPermission is used when nothing else is configured.
const DefaultPermission = PermissionReadOnly

var permissionRank = map[Permission]int{
	PermissionReadOnly:  0,
	PermissionSafeWrite: 1,
	PermissionFull:      2,
}

// ParsePermission normalizes a configured permission string.
//
// Empty input yields DefaultPermission. The machine-readable aliases used by
// other MCP database servers (read_only, safe_write, high_risk_write) are
// accepted so client configurations can be carried over.
func ParsePermission(s string) (Permission, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "":
		return DefaultPermission, nil
	case "read-only", "read_only", "readonly", "ro":
		return PermissionReadOnly, nil
	case "safe-write", "safe_write", "safewrite", "write", "rw":
		return PermissionSafeWrite, nil
	case "full", "high_risk_write", "high-risk-write", "all", "admin":
		return PermissionFull, nil
	default:
		return "", fmt.Errorf("unknown permission %q, want one of: read-only, safe-write, full", s)
	}
}

// Rank returns the ordering value of the permission, higher is more permissive.
func (p Permission) Rank() int {
	return permissionRank[p]
}

// Valid reports whether p is a known permission level.
func (p Permission) Valid() bool {
	_, ok := permissionRank[p]

	return ok
}

// Min returns the least permissive of p and other, which is how a global
// ceiling is combined with a per-connection grant. Composition can only ever
// restrict, never widen.
func (p Permission) Min(other Permission) Permission {
	if other.Rank() < p.Rank() {
		return other
	}

	return p
}

// CanWrite reports whether the permission allows any modifying statement.
func (p Permission) CanWrite() bool {
	return p.Rank() >= PermissionSafeWrite.Rank()
}

// Error codes are stable identifiers a client or model can branch on without
// depending on the exact wording of the message.
const (
	// CodePermissionDenied means the statement needs a higher permission level.
	CodePermissionDenied = "PERMISSION_DENIED"
	// CodeStatementRefused means the statement is refused at every level.
	CodeStatementRefused = "STATEMENT_REFUSED"
)

// StatementError is returned when a statement is rejected by the access policy.
type StatementError struct {
	// Code is a stable machine-readable identifier.
	Code string
	// Connection is the profile the statement was aimed at.
	Connection string
	// Kind is how dbq classified the statement.
	Kind Kind
	// Granted and Required are the effective and needed permission levels.
	// Required is empty when the statement is refused unconditionally.
	Granted  Permission
	Required Permission
	// Detail explains the specific problem, e.g. why a DELETE is unbounded.
	Detail string
	// Advice tells the caller what to do about it.
	Advice string
}

// Error deliberately never includes the SQL text. Errors flow to logs and to
// model context, and statements routinely carry data the caller should not have
// to think twice about echoing.
func (e *StatementError) Error() string {
	var b strings.Builder

	fmt.Fprintf(&b, "[%s] ", e.Code)

	if e.Required != "" {
		fmt.Fprintf(&b,
			"permission denied: this %s statement on connection %q requires %q access but %q is granted",
			e.Kind, e.Connection, e.Required, e.Granted,
		)
	} else {
		fmt.Fprintf(&b, "statement refused on connection %q", e.Connection)
	}

	if e.Detail != "" {
		fmt.Fprintf(&b, ". %s", e.Detail)
	}

	if e.Advice != "" {
		fmt.Fprintf(&b, ". %s", e.Advice)
	}

	return b.String()
}

// Authorize checks a statement against a granted permission level.
//
// It returns the analysis so the caller can decide between Query and Exec
// without re-parsing.
func Authorize(connection, sql string, granted Permission) (Analysis, error) {
	analysis := Analyze(sql)

	// Some statements cannot be made safe by any permission level: a batch,
	// because only its first statement was classified, and a connection-state
	// statement, because dbq hands out pooled connections.
	if analysis.Refused {
		return analysis, &StatementError{
			Code:       CodeStatementRefused,
			Connection: connection,
			Kind:       analysis.Kind,
			Granted:    granted,
			Detail:     analysis.Reason,
			Advice:     refusalAdvice(analysis),
		}
	}

	required := analysis.RequiredPermission()
	if granted.Rank() >= required.Rank() {
		return analysis, nil
	}

	return analysis, &StatementError{
		Code:       CodePermissionDenied,
		Connection: connection,
		Kind:       analysis.Kind,
		Granted:    granted,
		Required:   required,
		Detail:     analysis.Reason,
		Advice:     advice(analysis, granted),
	}
}

func refusalAdvice(analysis Analysis) string {
	if analysis.Kind == KindSession {
		return "Run it through a database client that owns its own connection"
	}

	return "Split the input and send one statement per call"
}

// advice turns a refusal into a next step, because a model reads the error at
// the moment it fails but may have long since lost the server instructions.
func advice(analysis Analysis, granted Permission) string {
	switch {
	case analysis.Unbounded && granted.CanWrite():
		return "Add a WHERE clause that selects specific rows, or ask the user to raise this connection's permission level"
	case analysis.Kind == KindSchema:
		return "Schema changes need the connection's permission level raised to full; this is an operator decision, not something to work around"
	default:
		return "This is a configuration decision. Ask the user to change the connection's permission level if the operation is genuinely needed"
	}
}
