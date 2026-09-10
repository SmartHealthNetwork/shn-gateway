package pgstore

// export_test.go re-exports the in-package test fixtures for the EXTERNAL test
// package (parity_test.go is `package pgstore_test`), so both halves of the
// package's tests drop and create the schema through one definition of the table
// list — a table added to gwTables is dropped by every test, not just the
// internal ones.
//
// Declared as vars, not funcs: a func named TestPool with a non-test signature
// would be rejected by `go test` as a malformed test function.
var (
	// DropGWTables drops every table EnsureSchema owns (see gwTables).
	DropGWTables = dropGWTables
	// TestPool returns a pool over a freshly created schema (pg-gated).
	TestPool = testPool
)
