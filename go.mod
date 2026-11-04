module github.com/SmartHealthNetwork/shn-gateway

go 1.26.0

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
	// Withdrawn: superseded by v0.52.0.
	[v0.50.0, v0.51.1]
	// Withdrawn: release tag identified the prior v0.42.0 snapshot instead of the intended release. Superseded by v0.43.1.
	v0.43.0
	// Withdrawn: returned 500 on clinician/patient attestation resumes against an SDC adaptive questionnaire (the amend refused an item whose group had not been delivered yet). Superseded by v0.37.1.
	v0.37.0
	// Withdrawn: bundled seed fixture carried a real payer's registry and subscriber identifiers, and source comments carried internal process vocabulary. Superseded by v0.36.2 — no behavior change between them.
	v0.36.1
	// Withdrawn: returned 422 on attestation resumes, and shipped internal process vocabulary in source comments. Superseded by v0.36.1.
	v0.36.0
	// Withdrawn: shipped internal process vocabulary in source comments. Same code as v0.34.1 — move to v0.34.1 or later.
	v0.34.0
)

require (
	github.com/SmartHealthNetwork/shn-sdk v1.6.0
	github.com/go-logr/logr v1.4.4
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/jackc/pgx/v5 v5.11.0
	github.com/prometheus/client_golang v1.24.1
	github.com/prometheus/otlptranslator v1.0.0
	github.com/samply/golang-fhir-models/fhir-models v0.3.2
	go.opentelemetry.io/contrib/instrumentation/runtime v0.72.0
	go.opentelemetry.io/contrib/otelconf v0.27.0
	go.opentelemetry.io/otel v1.47.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc v1.47.0
	go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp v1.47.0
	go.opentelemetry.io/otel/exporters/prometheus v0.69.0
	go.opentelemetry.io/otel/metric v1.47.0
	go.opentelemetry.io/otel/sdk v1.47.0
	go.opentelemetry.io/otel/sdk/metric v1.47.0
	go.opentelemetry.io/proto/otlp v1.11.1
	go.yaml.in/yaml/v3 v3.0.5
	golang.org/x/crypto v0.58.0
	golang.org/x/net v0.62.0
	google.golang.org/grpc v1.84.0
	google.golang.org/protobuf v1.36.12
)

require (
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cenkalti/backoff/v5 v5.0.3 // indirect
	github.com/cespare/xxhash/v2 v2.3.0 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/uuid v1.6.0 // indirect
	github.com/grpc-ecosystem/grpc-gateway/v2 v2.31.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/prometheus/client_model v0.6.3 // indirect
	github.com/prometheus/common v0.72.0 // indirect
	github.com/prometheus/procfs v0.22.0 // indirect
	go.opentelemetry.io/auto/sdk v1.2.1 // indirect
	go.opentelemetry.io/contrib/propagators/autoprop v0.72.0 // indirect
	go.opentelemetry.io/contrib/propagators/aws v1.47.0 // indirect
	go.opentelemetry.io/contrib/propagators/b3 v1.47.0 // indirect
	go.opentelemetry.io/contrib/propagators/jaeger v1.47.0 // indirect
	go.opentelemetry.io/contrib/propagators/ot v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc v0.23.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp v0.23.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/stdout/stdoutlog v0.23.0 // indirect
	go.opentelemetry.io/otel/exporters/stdout/stdoutmetric v1.47.0 // indirect
	go.opentelemetry.io/otel/exporters/stdout/stdouttrace v1.47.0 // indirect
	go.opentelemetry.io/otel/log v1.47.0 // indirect
	go.opentelemetry.io/otel/sdk/log v1.47.0 // indirect
	go.opentelemetry.io/otel/trace v1.47.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/exp v0.0.0-20260908205506-85c1c2202aba // indirect
	golang.org/x/sync v0.24.0 // indirect
	golang.org/x/sys v0.49.0 // indirect
	golang.org/x/text v0.43.0 // indirect
	google.golang.org/genproto/googleapis/api v0.0.0-20260928230214-8a89bd6388cc // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20260928230214-8a89bd6388cc // indirect
)
