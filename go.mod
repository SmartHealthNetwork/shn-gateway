module github.com/SmartHealthNetwork/shn-gateway

go 1.26

// Deleting a tag does NOT withdraw a Go module. Measured against the deleted
// v0.36.0: the GitHub tag 404s while proxy.golang.org still serves its .info
// and .zip and still lists the version, because the proxy is an immutable cache
// and sum.golang.org pins the hash permanently. `go get …@v0.36.0` works today.
// retract is the only mechanism that reaches a consumer: `go get` warns, and the
// version drops out of version selection. It is honored from the go.mod of a
// LATER release, which is why these land in v0.36.2.
retract (
	// Returned 422 on attestation resumes (its shn-sdk v0.43.0 pin duplicated an
	// amended QuestionnaireResponse answer), and shipped internal
	// design-document references in comments. Superseded by v0.36.1.
	v0.36.0
	// Carried internal design-document references in comments — none a secret,
	// none affecting behavior — that a line-based, case-sensitive sweep could
	// not see. Superseded by v0.36.2, which also replaces a partner's real payer
	// and subscriber identifiers in the bundled seed fixture with synthetic ones.
	v0.36.1
)

require (
	github.com/SmartHealthNetwork/shn-sdk v0.43.1
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
