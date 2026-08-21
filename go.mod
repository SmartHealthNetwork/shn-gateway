module github.com/SmartHealthNetwork/shn-gateway

go 1.26

// Deleting a tag does NOT withdraw a Go module. Measured against the deleted
// v0.36.0: the GitHub tag 404s while proxy.golang.org still serves its .info
// and .zip and still lists the version, because the proxy is an immutable cache
// and sum.golang.org pins the hash permanently. `go get …@v0.36.0` works today.
// retract is the only mechanism that reaches a consumer: `go get` warns, and the
// version drops out of version selection. It is honored from the go.mod of a
// LATER release, which is why these land in v0.36.2.
// A `go get` warning shows only the FIRST PHYSICAL LINE of the comment below each
// entry, so each reason is ONE physical line, however long — a wrapped reason clips
// mid-clause in the warning a consumer actually sees.
retract (
	// Withdrawn: shipped internal process vocabulary in source comments. Same code as v0.34.1 — move to v0.34.1 or later.
	v0.34.0
	// Withdrawn: returned 422 on attestation resumes, and shipped internal process vocabulary in source comments. Superseded by v0.36.1.
	v0.36.0
	// Withdrawn: bundled seed fixture carried a real payer's registry and subscriber identifiers, and source comments carried internal process vocabulary. Superseded by v0.36.2 — no behavior change between them.
	v0.36.1
)

require (
	github.com/SmartHealthNetwork/shn-sdk v0.44.0
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/jackc/pgx/v5 v5.10.0
	github.com/samply/golang-fhir-models/fhir-models v0.3.2
	golang.org/x/crypto v0.52.0
)

require (
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.20.0 // indirect
	golang.org/x/sys v0.45.0 // indirect
	golang.org/x/text v0.37.0 // indirect
)
