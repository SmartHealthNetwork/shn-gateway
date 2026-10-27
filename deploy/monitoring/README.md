# Monitoring extras

Ready-made pieces for watching an SHN Smart Gateway with your own monitoring stack. They read the
gateway's metrics as Prometheus stores them, from OTLP push (Prometheus' OTLP receiver) or from the
gateway's pull listener (`GET /metrics`); the gateway's monitoring guide,
[`docs/MONITORING.md`](../../docs/MONITORING.md), says how to turn each export on and catalogues
every metric. These files are examples to start from, outside the gateway's stability promise; the
metric names and attributes they read are inside it.

| File | What it is |
|---|---|
| `grafana/shn-gateway.json` | A Grafana dashboard: exchanges and refusals, refusals by rule and refusing party, exchange and backend latency (P50 and P95, a query each for explicit-bucket and exponential histograms), request and answer sizes, calls to your own system by class, origination legs, the store and its pool, the audit outbox, the validator lanes, involved patients omitted, the gateway's own exports, certificate expiry, the ingress signing key, the registrar feed's age and the peers it found, and Authorization Framework tokens. Pick the Prometheus data source, then the gateways (`job`) and replicas (`instance`). |
| `prometheus/alerts.yml` | Prometheus alert rules (below). |
| `prometheus/alerts_test.yml` | Their unit tests: `promtool test rules prometheus/alerts_test.yml`. |
| `compose.yml` | A local stack to try them: Prometheus with its OTLP receiver and the rules, and Grafana with the dashboard (`prometheus/prometheus.yml`, `grafana/provisioning/`). |
| `collector/*.yaml` | OpenTelemetry Collector recipes that forward the gateway's OTLP push to a vendor (below). |

## Alert rules

Each rule fires per gateway replica (`job`, `instance`), except the refusal rules, which add up a
gateway's replicas and fire per gateway (`job`) and rule. The thresholds are starting points.

| Alert | Fires when | Severity |
|---|---|---|
| `ShnGatewayBackendErrorRatio` | More than 5% of calls to your own system get no usable answer (a timeout, a connection or TLS failure, `auth`, a 5xx, an unreadable or malformed answer, or a failure that could not be classified) over 5 minutes, for 10 minutes, while there is at least one call a minute | warning |
| `ShnGatewayP95NearLegBudget` | An exchange kind's P95, over 5 minutes, is above 25 s for 10 minutes. The leg budget is 30 s and the gateway's deadline for your system is 25 s, so it fires before callers see 504s | warning |
| `ShnGatewayAuditDrops` | The gateway dropped an audit record in the last 10 minutes | page |
| `ShnGatewayTelemetryExportFailures` | An OTLP export failed, or was accepted with data points rejected, in the last 15 minutes | warning |
| `ShnGatewayValidatorLaneDown` | A validator lane has stayed unqualified for 10 minutes | warning |
| `ShnGatewayCertificateExpiry` | A server certificate (what an endpoint the gateway calls presented) or the gateway's own expires within 21 days, for 5 minutes; a trust-root bundle is left out | warning |
| `ShnGatewayIngressKeySlack` | The ingress signing key expires within an hour: the key rotates an hour early, so this is a stalled rotation | page |
| `ShnGatewayRegistrarFeedStale` | The registrar feed has not been read for over 300 s, for 3 minutes. It carries registrations and revocations, so a revocation does not reach the gateway meanwhile | warning |
| `ShnGatewayRefusedFidelityOrAudit` | A `fidelity` or `audit` refusal in the last 5 minutes: the gateway's own edit of a message was refused, or an exchange's audit could not be recorded | warning |
| `ShnGatewayRefusedIntegrityOrRouting` | At least three `integrity` or `routing` refusals in each of two of the last three 5-minute periods: a run of failed integrity checks, or exchanges the network could not route or address | warning |

A refusal rule counts a counter it has no earlier value for in full, so that a gateway's first
refusals under a rule count. So when Prometheus first sees a counter that already holds refusals
from before Prometheus watched the gateway, it counts them in the five minutes after that first
sample, as if they were new; a counter back after more than a day without a sample can do the same.
`ShnGatewayRefusedFidelityOrAudit` then fires for up to about five minutes from each such first
sample. `ShnGatewayRefusedIntegrityOrRouting` fires if those old refusals, with new ones or with
another replica's old ones, reach three in two of the last three periods.

As of v1.0.0 nothing appends to the gateway's audit outbox yet, so `ShnGatewayAuditDrops`, and
the dashboard's audit append, retry and drop panels, stay silent until a release that writes
audit records: until then their silence means not measured, not no drops.

A push that fails cannot report its own failure over that push: `ShnGatewayTelemetryExportFailures`
fires where the export self-metric reaches Prometheus another way, such as the pull listener or a
second exporter. `GET /internal/checks` reports the same outcome as `telemetry-export`.

## Collector recipes

Each recipe receives the gateway's OTLP push (point the gateway at the Collector:
`OTEL_METRICS_EXPORTER=otlp`, `OTEL_EXPORTER_OTLP_ENDPOINT=http://<collector>:4318`) and forwards
it to one vendor. Keys and endpoints come from the Collector's environment, named at the top of
each file; none is in the files.

| File | Vendor |
|---|---|
| `collector/datadog.yaml` | Datadog. Set the gateway's `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=delta`, as Datadog prefers |
| `collector/splunk.yaml` | Splunk Enterprise or Splunk Cloud Platform, through the HTTP Event Collector to a metrics index |
| `collector/newrelic.yaml` | New Relic over OTLP (the recipe turns cumulative into the delta New Relic prefers) |
| `collector/azure-monitor.yaml` | Azure Monitor (Application Insights) |
| `collector/aws-adot.yaml` | Amazon Managed Service for Prometheus, through the AWS Distro for OpenTelemetry or the contrib Collector |
| `collector/grafana-cloud.yaml` | Grafana Cloud over OTLP |

The recipes run on `otel/opentelemetry-collector-contrib` 0.162.0. The AWS recipe uses only
components the AWS Distro for OpenTelemetry also carries, but it is tested on the contrib
Collector. SHN's monitoring gate checks that each one validates and forwards the gateway's
metrics in its vendor's format to a local stand-in; delivery to the vendor itself is not tested.
