# Monitoring the gateway

This page covers every way to watch a Smart Gateway: what it reports, where, and how to
read it. Each setting has its own row in [CONFIGURATION.md](CONFIGURATION.md); this page
links to those rows rather than repeating them. Every monitoring setting is off unless
you set it, and nothing on this page can change, delay or refuse an exchange. None of
these settings stops the gateway from booting, apart from `OBSERVER_ADDR`, which must
name a loopback address or the gateway does not start.

## What the gateway reports, at a glance

| Surface | What it tells you | Turn it on |
|---|---|---|
| `GET /health` | The gateway is up and serving, and its version. It never reflects monitoring export. | Always on |
| `GET`/`POST /internal/checks` | Probes of every outbound dependency, and the two monitoring checks | Always on; `CHECKS_TOKEN` for callers off the gateway's host |
| `GET /internal/stats` | Exchange counts, outcomes and latency quantiles over the last 1, 5 and 60 minutes, per replica | Always on; access as for `/internal/checks` |
| Access lines | One JSON line per exchange: each call from your own system on a Da Vinci ingress route or a module 1 origination route, and each leg from the network | Always on (standard error) |
| CloudWatch EMF | Exchange, backend, operational and validator-lane metrics, as EMF lines on standard output | `METRICS_SERVICE` |
| OTLP push | The same metrics as OpenTelemetry instruments, with the Go runtime's and the process's, pushed to a Collector or a vendor | `OTEL_METRICS_EXPORTER=otlp` |
| Prometheus pull | The same instruments, for a Prometheus server to scrape, on a listener of their own | `OTEL_METRICS_EXPORTER=prometheus` (or `otlp,prometheus`) |
| Configuration file | The OTLP push and the Prometheus pull, set from one OpenTelemetry configuration file in place of the `OTEL_*` settings | `OTEL_CONFIG_FILE` |
| Observer stream | A live stream of the gateway's own events, payloads included, for local tooling | `OBSERVER_ADDR` (loopback only) |

The observer stream is not a monitoring feed: its events carry the payloads flowing
through the gateway's edge, so it binds only to loopback and suits local tooling such as
the SHN Kit's flow inspector ([CONFIGURATION.md](CONFIGURATION.md#observer-stream-optional--local-tooling)).
Build dashboards and alerts from the metrics and access lines below.

## Health and checks

`GET /health` answers while the gateway is serving, with the version it runs. It reads
nothing from monitoring: an export that fails, or a monitoring setting that is not
valid, never changes it.

Its `capability-declaration` check (from v1.1.0, with a registrar) says what your gateway's
declaration of the legs it takes came to, and from v1.8.0, for a payer, of the request frames it
serves. Its `status` is always `ok`, because a gateway that has not declared serves as before.
`lastSuccess` is when the registrar stored it. For a holder founded with the network, the
registrar stores only the request frames, and its legs stay those it was founded with. `lastError`
says why it has not been: `failing` (being retried), `not-applied` (a `409`, not resent), or
`registrar-takes-no-declaration` (a `404` or `405`, tried hourly). Neither is set before the
first answer. The network's Hub refuses a marked leg when the list the registrar holds lacks it,
so read this check if a provider reports that `422` for a leg your gateway should take.

`GET /internal/checks` returns the newest run of the gateway's probes of its outbound
dependencies, and `POST` runs them again
([CONFIGURATION.md](CONFIGURATION.md#operational-checks-optional)). Two of its results
report on monitoring itself:

- `telemetry-config`, whenever a monitoring setting or any other `OTEL_*` setting is
  set: it fails with `invalid-telemetry-configuration` when one is not valid, and its
  `hint` names the settings, never their values.
- `telemetry-export`, while OTLP export is on: the newest export's outcome, read
  without contacting the vendor (`not-checked` until the first export has run).

Both carry `"advisory": true`: a failing advisory result means some monitoring is off or
not delivered, never that the gateway is not ready to carry exchanges. A result that ran
no probe, these two among them, carries `"noProbe": true` and no latency.

## Exchange stats

`GET /internal/stats` summarises the exchanges this replica answered: for each direction
and exchange kind, the count, the rate, the outcomes and the latency at P50, P95 and P99,
over the last 1, 5 and 60 complete minutes plus the current one, and, by exchange, the
calls to your own system. It needs
no setting and no metrics backend, and it carries no payload, no peer's identity and no
identifier. Each replica counts for itself, in memory, from its start.

A call on one of module 1's origination routes (`route` `scenario-*`) counts here, with its
outcome, but adds no latency: it runs a flow of legs, some of which wait on a person, so its
time is not one exchange's answer time. For the same reason it records no
`shn.gateway.exchange.duration` (`ExchangeLatency`) and no
`shn.gateway.exchange.stage.duration` (`ExchangeStageLatency`); its access line keeps the
whole call's `latencyMs`, and each of its legs still counts in `shn.gateway.legs`
(`LegOutcome`).

`shn gateway stats`, in the `shn` CLI, reads several replicas and merges them exactly: give
it each replica's base URL (`--url`, repeatable) and the `CHECKS_TOKEN` in a file
(`--token-file`). The answer's fields, and how to merge replicas yourself, are in
[CONFIGURATION.md](CONFIGURATION.md#exchange-stats-internalstats).

## Access lines

Every exchange writes one line to standard error, at every conformance level: each call
from your own system on a Da Vinci ingress route, each call on one of module 1's
origination routes (a `/scenario` route that builds and sends a request; `route` is
`scenario-*`, the route's path joined by `-`, such as `scenario-uc07-complete`), and each
leg the gateway receives from the network. The local `/scenario` routes (`uc06/cancel`,
`uc07/cancel`, `uc07/pending`, `reset`), `/health`, `/internal/*` and the gateway's other
routes write none.

```
2026/10/25 12:00:00 gateway: access: {"time":…,"correlationId":…,"direction":"ingress","exchange":…,"outcome":…,"status":…,"latencyMs":…,…}
```

`direction` is `ingress` for a call from your own system, which the gateway carries to the
network (an origination route's call included: the gateway sends its leg), and `inbound`
for a leg the gateway receives from the network. An origination call can send several
legs: its line's `exchange` and `correlationId` are the first leg's, while a refusal it
records may belong to a later leg of the same call. A fidelity refusal's `refusal` names its
`stage`: `build`, the gateway refused to build the message the refusal is about because of
what the caller declared or did not state, or
`verify`, an edit the gateway built failed its own verification. The line starts
with the date and time Go's log package writes, so a query first strips everything up to
`gateway: access: ` and reads the rest as JSON. Every field is described in
[CONFIGURATION.md](CONFIGURATION.md#access-lines-always-on).

### Per-party queries

The party a line is about is `recipient` on an `ingress` line (the gateway called out to
it) and `sender` on an `inbound` line. The queries below count calls, answers, refusals
and failures, with P50 and P95 latency, by party, direction and exchange, then refusals by
rule and your own system's answers by party.

**CloudWatch Logs Insights.** A hosted gateway's log group is `/shn-cloud/<tenant>`.
These queries were run on a hosted gateway's log group.

```
filter @message like "gateway: access: "
| parse @message "gateway: access: *" as raw
| fields jsonParse(raw) as a
| fields a.direction as direction, a.exchange as exchange, a.outcome as outcome,
         toNumber(a.latencyMs) as latencyMs,
         if(a.direction = "ingress", a.recipient, a.sender) as peer
| stats count(*) as calls,
        sum(if(outcome = "answered", 1, 0)) as answered,
        sum(if(outcome = "refused", 1, 0)) as refused,
        sum(if(outcome = "unreachable" or outcome = "upstream-error", 1, 0)) as failed,
        pct(latencyMs, 50) as p50ms, pct(latencyMs, 95) as p95ms
  by peer, direction, exchange
| sort calls desc
```

From v1.1.0 an outage of the Authorization Framework is `refused` on `authority`
here, with `authorizeError` set, not `unreachable`: no authority could be
obtained. Alert on `LegError` for it, whose leg outcome is still `unreachable`.

From shn-gateway v1.10.0, a redirect answered by a network service (discovery and its
trust-anchor keys, the Hub,
the Authorization Framework, consent, the registrar, the audit endpoint), the OTLP
collector or a validator or certification sidecar is never followed: the call fails as
when that endpoint cannot be reached, and the class `redirect-refused` tells it apart from
an outage. It should never happen; when it does, a URL names an address that redirects
instead of its final one. It appears in the boot error (discovery, a trust-anchor key, the
registrar's feed), as the access line's `authorizeError` or `answerError` (the
Authorization Framework), in the log lines `gateway: hub leg … failed: redirect-refused
(HTTP <status>)`, `gateway: consent check failed: redirect-refused (HTTP <status>)` and
`gateway: capability declaration not stored (redirect-refused) …`, as the
`AuditAppendRetried` reason `redirect-refused`, and as an `http-status` hint naming the
status and `redirect-refused` on `telemetry-export` (below) and on the `/internal/checks`
probe of any of those endpoints. The clients that reach a participant's own system keep
their own redirect rule (CONFIGURATION.md, the access line's `redirects`). See
CONFIGURATION.md, "Network services and sidecars follow no redirect".

A patient an exchange's involved list left out has no record of it under that
patient. From v1.1.0 the access line's `involvedOmitted` counts them by reason, on
any gateway. With metrics on, `InvolvedOmitted` is the same count. Alert on either.

Refusals, by who refused and the network rule:

```
filter @message like "gateway: access: " and @message like /"outcome":"refused"/
| parse @message "gateway: access: *" as raw
| fields jsonParse(raw) as a
| fields if(a.direction = "ingress", a.recipient, a.sender) as peer,
         a.direction as direction, a.exchange as exchange,
         a.refusal.by as refusedBy, a.refusal.rule as rule, a.status as status
| stats count(*) as refusals by peer, direction, exchange, refusedBy, rule, status
| sort refusals desc
```

Your own system's answers on legs from the network, by the party whose leg it served:

```
filter @message like "gateway: access: " and @message like /"direction":"inbound"/
| parse @message "gateway: access: *" as raw
| fields jsonParse(raw) as a
| filter ispresent(a.backend.status)
| fields a.sender as peer, a.exchange as exchange,
         toNumber(a.backend.latencyMs) as backendMs, coalesce(a.backend.errorClass, "none") as errorClass
| stats count(*) as calls, pct(backendMs, 50) as p50ms, pct(backendMs, 95) as p95ms
  by peer, exchange, errorClass
| sort calls desc
```

A regular-expression filter in Logs Insights takes a `/…/` literal, not a quoted string.

**Loki** (LogQL; `job` is whatever label your log shipper gives the gateway). These
queries were run on Loki 3.4 against synthetic access lines in the gateway's format.

```
sum by (peer, direction, exchange, outcome) (
  count_over_time(
    {job="shn-gateway"} |= "gateway: access: "
      | pattern "<_> <_> gateway: access: <access>"
      | line_format "{{.access}}"
      | json direction="direction", exchange="exchange", outcome="outcome", sender="sender", recipient="recipient"
      | label_format peer=`{{ if eq .direction "ingress" }}{{ .recipient }}{{ else }}{{ .sender }}{{ end }}`
  [1h])
)
```

P95 latency in milliseconds:

```
quantile_over_time(0.95,
  {job="shn-gateway"} |= "gateway: access: "
    | pattern "<_> <_> gateway: access: <access>"
    | line_format "{{.access}}"
    | json direction="direction", exchange="exchange", sender="sender", recipient="recipient", latencyMs="latencyMs"
    | label_format peer=`{{ if eq .direction "ingress" }}{{ .recipient }}{{ else }}{{ .sender }}{{ end }}`
    | unwrap latencyMs
[1h]) by (peer, direction, exchange)
```

Refusals, by who refused and the network rule:

```
sum by (peer, direction, exchange, refused_by, rule) (
  count_over_time(
    {job="shn-gateway"} |= "gateway: access: " |= `"outcome":"refused"`
      | pattern "<_> <_> gateway: access: <access>"
      | line_format "{{.access}}"
      | json direction="direction", exchange="exchange", sender="sender", recipient="recipient", refused_by="refusal.by", rule="refusal.rule"
      | label_format peer=`{{ if eq .direction "ingress" }}{{ .recipient }}{{ else }}{{ .sender }}{{ end }}`
  [1h])
)
```

**Splunk** (SPL). These queries have not been run against a Splunk instance; they are
written to Splunk's documented `rex`, `spath`, `eval` and `stats` behaviour.

```
index=<your gateway index> "gateway: access: "
| rex field=_raw "gateway: access: (?<access>\{.*\})$"
| spath input=access
| eval peer=if(direction=="ingress", recipient, sender)
| stats count AS calls,
        count(eval(outcome=="answered")) AS answered,
        count(eval(outcome=="refused")) AS refused,
        count(eval(outcome=="unreachable" OR outcome=="upstream-error")) AS failed,
        perc50(latencyMs) AS p50ms, perc95(latencyMs) AS p95ms
  BY peer, direction, exchange
| sort - calls
```

The note on an outage of the Authorization Framework under the first CloudWatch
query holds here too: from v1.1.0 it is `refused`, not `failed`.

Refusals, by who refused and the network rule:

```
index=<your gateway index> "gateway: access: " "\"outcome\":\"refused\""
| rex field=_raw "gateway: access: (?<access>\{.*\})$"
| spath input=access
| eval peer=if(direction=="ingress", recipient, sender), refused_by='refusal.by', rule='refusal.rule'
| stats count AS refusals BY peer, direction, exchange, refused_by, rule, status
| sort - refusals
```

The metrics below never carry a party: break traffic down by party from the access
lines.

## Metrics

### CloudWatch EMF

With `METRICS_SERVICE` set, the gateway writes its metrics to standard output in
CloudWatch embedded metric format, one line per measurement, in the namespace
`METRICS_NAMESPACE` with the `Env` dimension from `METRICS_ENV`
([CONFIGURATION.md](CONFIGURATION.md#exchange-metrics-optional--cloudwatch-emf)). Your
log agent ships them; CloudWatch extracts the metrics. Without it the gateway writes no
EMF line. The 18 EMF metrics written before OpenTelemetry export was added, among them
v1.0.0's validator-lane and audit-outbox metrics, are unchanged, byte for byte; the
others sit beside them.

### OTLP push

With `OTEL_METRICS_EXPORTER=otlp` the gateway pushes every instrument in the catalogue
below to the OTLP endpoint you set, every minute by default
([CONFIGURATION.md](CONFIGURATION.md#opentelemetry-metrics-export-optional)). The
resource names the gateway (`service.name`, `service.version`, `service.instance.id`,
`deployment.environment.name`, `shn.holder_id`, `shn.role`). It is independent of EMF:
each, both or neither.

The gateway follows the OpenTelemetry specification for these settings except in two
documented ways: export is off unless `OTEL_METRICS_EXPORTER` names it, and a setting
that is not valid never stops the gateway or sends metrics to a guessed place. Where a
setting decides where metrics go or how they authenticate, and no valid form of it is
left, OTLP export turns off instead of falling back to the localhost default.

Collector recipes for Datadog, Splunk, New Relic, Azure Monitor, AWS (on the contrib Collector; ADOT carries the same components) and
Grafana Cloud are in `deploy/monitoring/collector/`, listed in the
[monitoring examples README](../deploy/monitoring/README.md#collector-recipes). Datadog
prefers delta temporality (`OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=delta`).

### Prometheus pull

With `prometheus` in `OTEL_METRICS_EXPORTER`, the gateway serves `GET /metrics` on a
listener of its own, never on the gateway's listener (default `localhost:9464`; the
`OTEL_EXPORTER_PROMETHEUS_HOST` and `OTEL_EXPORTER_PROMETHEUS_PORT` rows). On a loopback
bind nothing more is needed: run the scraper, or a Collector's `prometheus` receiver, on
the same host.

Off loopback, `0.0.0.0` included, the listener serves only over TLS
(`METRICS_PULL_TLS_CERT_FILE`, `METRICS_PULL_TLS_KEY_FILE`) and only with a scrape
credential: a bearer token (`METRICS_PULL_TOKEN`), a client certificate signed by
`METRICS_PULL_CLIENT_CA_FILE`, or both. Without them nothing is served there, and
`telemetry-config` says why. A Prometheus scrape job for that case:

```yaml
scrape_configs:
  - job_name: shn-gateway
    scheme: https
    authorization:
      credentials_file: /etc/prometheus/shn-gateway-token
    tls_config:
      ca_file: /etc/prometheus/shn-gateway-ca.pem
    static_configs:
      - targets: ["gateway.internal:9464"]
```

A scrape is always cumulative and always has SHN's histogram buckets: the temporality
and histogram settings apply to OTLP push only. A scrape also carries `target_info`,
the resource's attributes as labels.

### Configuration file

With `OTEL_CONFIG_FILE` set, the gateway reads an OpenTelemetry configuration file
(`file_format` 1.0) and ignores the other `OTEL_*` settings, `OTEL_SDK_DISABLED`
included. The file builds the same export as the settings, with the same checks,
defaults and boot lines:
- one `periodic` reader with an `otlp_http` or `otlp_grpc` exporter is the OTLP push;
- one `pull` reader with a `prometheus/development` exporter is the pull listener,
  protected by the same `METRICS_PULL_*` settings, which the file has no place for;
- the file's `resource` and `views` apply over the gateway's own; the resource cannot set
  `service.version` or any `shn.*` attribute.

A second reader of either kind, a `console` or other exporter, and a setting that is not
valid are reported through `telemetry-config` and turned off on their own. Some other keys
are ignored and reported, and `tracer_provider`, `logger_provider` and `propagator` are
named in a boot warning and ignored. The gateway never falls back to the other settings. What the file can hold is in
[CONFIGURATION.md](CONFIGURATION.md#opentelemetry-metrics-export-optional), "The
configuration file".

## Catalogue

Every instrument the gateway exports, under each of its names. The Prometheus name is
the OpenTelemetry name translated with underscore escaping and suffixes: dots become
underscores, a counter gains `_total`, and a unit of seconds, bytes or percent gains
`_seconds`, `_bytes` or `_percent`; an attribute becomes the label of the same name, its
dots underscores. An EMF metric named twice is a line per measurement plus a rollup or a
narrower copy over it. Every attribute value comes from a closed list: a value the list
does not name is recorded as `other`, or as `_OTHER` for `http.server.request.duration`,
whose attributes are the OpenTelemetry semantic conventions' own. No attribute carries a
participant, an identifier, a URL or free text.

The last eleven rows are the Go runtime's (`go.*`) and the process's (`process.*`),
which every exporter carries beside the gateway's own instruments. They are
OpenTelemetry only, with no EMF, and their names, units, attributes and values
(`go.memory.type` `stack` or `other`, `cpu.mode` `user` or `system`) are the semantic
conventions'. `go.memory.limit` is exported only where a Go memory limit is set
(`GOMEMLIMIT`), and `process.memory.usage` only where the gateway can read its resident
memory (Linux).

| OpenTelemetry | Prometheus | EMF | Kind | Unit | Attributes | What it measures |
|---|---|---|---|---|---|---|
| `shn.gateway.exchanges` | `shn_gateway_exchanges_total` | `Exchange` | counter | `{exchange}` | `shn.direction`, `shn.exchange`, `shn.outcome` | Exchanges this gateway answered, both directions. |
| `shn.gateway.exchange.duration` | `shn_gateway_exchange_duration_seconds` | `ExchangeLatency` | histogram | `s` | `shn.direction`, `shn.exchange` | Time from an exchange's arrival to its answer. |
| `shn.gateway.backend.calls` | `shn_gateway_backend_calls_total` | `BackendCall`, `BackendError` | counter | `{call}` | `shn.exchange`, `shn.backend.class` | Calls to the participant's own system, by class. |
| `shn.gateway.backend.duration` | `shn_gateway_backend_duration_seconds` | `BackendLatency` | histogram | `s` | `shn.exchange` | Duration of a call to the participant's own system. |
| `shn.gateway.legs` | `shn_gateway_legs_total` | `LegOutcome`, `LegError` | counter | `{leg}` | `shn.outcome` | Origination leg events, by outcome. |
| `shn.gateway.store.errors` | `shn_gateway_store_errors_total` | `StoreError` | counter | `{error}` | `shn.store` | Shared-state store failures, by store. |
| `shn.gateway.involved.omitted` | `shn_gateway_involved_omitted_total` | `InvolvedOmitted` | counter | `{patient}` | `shn.reason` | Involved patients a leg left out of its list, by reason. |
| `shn.gateway.store.pool.connections` | `shn_gateway_store_pool_connections` | `StorePool` (`stat` `TotalConns`, `AcquiredConns`, `IdleConns`) | gauge | `{connection}` | `shn.pool.state` | Shared-state pool connections, by state. |
| `shn.gateway.store.pool.empty_acquires` | `shn_gateway_store_pool_empty_acquires_total` | `StorePool` (`stat` `EmptyAcquireCount`) | counter | `{acquire}` | none | Shared-state pool acquires that waited for a connection. |
| `shn.gateway.audit.appends` | `shn_gateway_audit_appends_total` | `AuditAppendSent` | counter | `{record}` | none | Audit records the Audit Plane accepted. No producer in v1.0.0. |
| `shn.gateway.audit.retries` | `shn_gateway_audit_retries_total` | `AuditAppendRetried` | counter | `{record}` | `shn.reason` | Audit record sends retried, by reason (`unknown-sender`, `unavailable`, `unreachable`, `store`, `redirect-refused`: the audit endpoint answered a redirect, which is not followed). No producer in v1.0.0. |
| `shn.gateway.audit.drops` | `shn_gateway_audit_drops_total` | `AuditAppendDroppedBy`, `AuditAppendDropped` | counter | `{record}` | `shn.reason` | Audit records that left the outbox without the Audit Plane's acceptance, by reason. No producer in v1.0.0. |
| `shn.gateway.audit.queue.depth` | `shn_gateway_audit_queue_depth` | `AuditQueueDepth` | gauge | `{record}` | none | Audit records queued in the outbox. No producer in v1.0.0. |
| `shn.gateway.audit.endpoint.unknown` | `shn_gateway_audit_endpoint_unknown` | `AuditEndpointUnknown` | gauge | `{record}` | none | Audit records queued while discovery names no audit endpoint. No producer in v1.0.0. |
| `shn.gateway.validator.lane.qualified` | `shn_gateway_validator_lane_qualified` | `ValidatorLane` | gauge | none | `shn.ig_line`, `shn.lane` | 1 when a validator lane is qualified, else 0. |
| `shn.gateway.validator.lane.attempts` | `shn_gateway_validator_lane_attempts_total` | `ValidatorLaneAttempt` | counter | `{attempt}` | `shn.ig_line`, `shn.lane`, `shn.outcome` | Default validator lane qualification attempts, by outcome. |
| `shn.gateway.info` | `shn_gateway_info` | `GatewayInfo` | gauge | none | `shn.wire_protocol`, `shn.conformance.level`, `shn.ig_line` | 1 for each IG line this gateway declares, with its wire protocol and conformance level. |
| `shn.gateway.certificate.expiry` | `shn_gateway_certificate_expiry_seconds` | `CertificateExpiry`, `CertificateExpiryMin` | gauge | `s` | `shn.endpoint.kind`, `shn.cert.role`, `shn.ig_line` | Seconds until the soonest certificate of each endpoint kind and role expires. |
| `shn.gateway.ingress_key.remaining` | `shn_gateway_ingress_key_remaining_seconds` | `IngressKeyRemaining` | gauge | `s` | none | Seconds until the current ingress signing key's not-after. |
| `shn.gateway.ingress_key.age` | `shn_gateway_ingress_key_age_seconds` | `IngressKeyAge` | gauge | `s` | none | Age of the newest ingress signing key, in seconds. |
| `shn.gateway.dependency.age` | `shn_gateway_dependency_age_seconds` | `DependencyAge`, `DependencyAgeMax` | gauge | `s` | `shn.dependency` | Seconds since a dependency was last read successfully. |
| `shn.gateway.peers.known` | `shn_gateway_peers_known` | `PeersKnown` | gauge | `{peer}` | none | Holders the last successful registrar read converged. |
| `shn.gateway.authorization.token.duration` | `shn_gateway_authorization_token_duration_seconds` | `TokenLatency` | histogram | `s` | none | Time to acquire an Authorization Framework token, its retry included. |
| `shn.gateway.authorization.tokens` | `shn_gateway_authorization_tokens_total` | `TokenAcquisition` | counter | `{token}` | `shn.outcome` | Authorization Framework token acquisitions, by outcome. |
| `shn.gateway.telemetry.exports` | `shn_gateway_telemetry_exports_total` | none | counter | `{export}` | `shn.outcome` | OTLP metric exports, by outcome. |
| `shn.gateway.refusals` | `shn_gateway_refusals_total` | `ExchangeRefused`, `ExchangeRefusedByRule` | counter | `{refusal}` | `shn.direction`, `shn.exchange`, `shn.refusal.rule`, `shn.refusal.by`, `shn.refusal.stage` | Exchanges refused, by the network rule, the party that refused and, for a fidelity refusal, its stage. |
| `shn.gateway.exchange.stage.duration` | `shn_gateway_exchange_stage_duration_seconds` | `ExchangeStageLatency` | histogram | `s` | `shn.direction`, `shn.exchange`, `shn.stage` | Time an exchange spent in each stage; an exchange's stages sum to its duration. |
| `shn.gateway.active_requests` | `shn_gateway_active_requests` | `ActiveRequests` (peak each minute) | updowncounter | `{request}` | `shn.direction` | Exchanges in flight. In EMF, the peak each minute. |
| `shn.gateway.exchange.request.size` | `shn_gateway_exchange_request_size_bytes` | `RequestBytes` | histogram | `By` | `shn.direction`, `shn.exchange` | Size of an exchange's request body, when it was read to its end. |
| `shn.gateway.exchange.response.size` | `shn_gateway_exchange_response_size_bytes` | `ResponseBytes` | histogram | `By` | `shn.direction`, `shn.exchange` | Size of the answer body an exchange wrote; 0 on a call that came as a `HEAD`, whose answer carries no body (from shn-gateway v1.12.0). |
| `shn.gateway.conformance.findings` | `shn_gateway_conformance_findings_total` | `ConformanceFinding` | counter | `{finding}` | `shn.exchange`, `shn.conformance.level`, `shn.finding.kind`, `shn.finding.action`, `shn.finding.tolerated` | Conformance findings recorded, by level, kind, the decision recorded and whether the peer was tolerated. |
| `shn.gateway.validator.duration` | `shn_gateway_validator_duration_seconds` | `ValidatorLatency` | histogram | `s` | `shn.ig_line`, `shn.lane` | Duration of a $validate call, by IG line and lane. |
| `shn.gateway.conformance.checks.dropped` | `shn_gateway_conformance_checks_dropped_total` | `ObserveCheckDropped` | counter | `{check}` | none | Observe checks dropped because the observe queue was full or closed. |
| `shn.gateway.listener.cuts` | `shn_gateway_listener_cuts_total` | `ListenerCut` | counter | `{cut}` | `shn.listener.bound`, `shn.listener` | Connections (HTTP/1.1) or streams (HTTP/2) a listener cut at one of its connection bounds, by bound and listener. |
| `http.server.request.duration` | `http_server_request_duration_seconds` | none | histogram | `s` | `http.request.method`, `http.response.status_code`, `http.route`, `url.scheme`, `network.protocol.version`, `error.type` | Duration of HTTP server requests. |
| `go.memory.used` | `go_memory_used_bytes` | none | updowncounter | `By` | `go.memory.type` | Memory used by the Go runtime, by type. |
| `go.memory.limit` | `go_memory_limit_bytes` | none | updowncounter | `By` | none | The Go runtime's memory limit, where one is set. |
| `go.memory.allocated` | `go_memory_allocated_bytes_total` | none | counter | `By` | none | Memory allocated to the heap. |
| `go.memory.allocations` | `go_memory_allocations_total` | none | counter | `{allocation}` | none | Heap allocations. |
| `go.memory.gc.goal` | `go_memory_gc_goal_bytes` | none | updowncounter | `By` | none | Heap size target for the end of the garbage collection cycle. |
| `go.goroutine.count` | `go_goroutine_count` | none | updowncounter | `{goroutine}` | none | Live goroutines. |
| `go.processor.limit` | `go_processor_limit` | none | updowncounter | `{thread}` | none | OS threads that can run Go code at once. |
| `go.config.gogc` | `go_config_gogc_percent` | none | updowncounter | `%` | none | The garbage collector's heap target percentage (`GOGC`), 100 unless set. |
| `process.cpu.time` | `process_cpu_time_seconds_total` | none | counter | `s` | `cpu.mode` | CPU time the process has used, by mode. |
| `process.memory.usage` | `process_memory_usage_bytes` | none | updowncounter | `By` | none | The process's resident memory, on Linux. |
| `process.uptime` | `process_uptime_seconds` | none | gauge | `s` | none | Time since the process started. |

The five audit-outbox instruments (`shn.gateway.audit.*`) have no producer in v1.0.0:
nothing appends to the outbox yet, so they stay silent, and their silence means not
measured, not no drops ([CONFIGURATION.md](CONFIGURATION.md#the-audit-outbox)).

### Connection cuts

`shn.gateway.listener.cuts` (EMF `ListenerCut`, dimensions `bound` and `listener`)
counts each cut a listener made at one of its connection bounds
([DEPLOYMENT.md](DEPLOYMENT.md#connection-bounds)). Over HTTP/1.1 a cut ends the
connection; over HTTP/2 it ends one stream, and the connection's other streams go
on. `shn.listener` is `main` or
`observer`; `shn.listener.bound` is:

- `header`: the request headers, or the TLS handshake, did not complete within 10 s;
- `body-stall`: a request body sent no byte for 30 s;
- `body-ceiling`: a request body was still arriving after 10 min;
- `write-stall`: the client did not take the next 8 KiB of the answer within 30 s
  (one stream over HTTP/2), or, over HTTP/2, the connection took no frame (at most
  about 8 KiB) within 30 s: that cut ends the connection and every stream on it,
  and counts once;
- `attachment-arrival`: a documentation attachment submission did not arrive
  within its own 30 s bound.

Only `header` occurs on the observer. Each cut also writes one line:

```
gateway: listener: cut a connection at its <bound> bound (listener=<listener>)
```

The line names the bound and the listener only: never the request's path, its
headers or the client's address. It is written whether or not metrics are exported.
A TLS handshake cut at the header bound also produces Go's own line,
`http: TLS handshake error from <address>: … i/o timeout`, which does name the
client's address, as Go writes it for every failed handshake.

Some closes are not counted:

- an idle kept-alive connection closed after 120 s, which is the normal end of a
  connection;
- over HTTP/2, a stream whose headers never complete, which Go's HTTP/2 server ends
  with the idle bound itself;
- over HTTP/2, a connection whose client never sends the HTTP/2 preface, which Go's
  HTTP/2 server closes on its own timer;
- over HTTP/2, a stream reset because the client stopped granting it flow-control
  window for the last part of an answer, sent after the gateway's handling of the
  call ended (a connection that stops taking bytes altogether is counted).

Over HTTP/2, the deadline for an answer's final flush can reach Go's HTTP/2 server
after the stream has ended. Go then resets the already closed stream (`RST_STREAM`)
once that deadline passes, 30 s later, or 60 s when the request's body was left
unread. Clients ignore the reset, and nothing is cut or counted.

A cut is not an exchange refusal, and no exchange records one. A rising count means
clients that are slow or have stalled (a congested link, a client that crashed
mid-request), or a deliberate attempt to hold connections open. A steady trickle of
`header` cuts on a gateway that faces the internet directly is ordinary background
scanning.

Each EMF metric's stability class is in [STABILITY.md](../STABILITY.md#metrics). The
OpenTelemetry export, its instrument names and their attributes are in the evolving
`monitoring` class.

## What EMF costs

With `METRICS_SERVICE` set, CloudWatch bills the gateway's EMF output twice:
as log data ingested, by the bytes the lines take, and as custom metrics, one
for each distinct combination of a metric and its dimensions. The figures
below are estimates at AWS's list prices for US East (N. Virginia), read from
[Amazon CloudWatch pricing](https://aws.amazon.com/cloudwatch/pricing/) on
2026-10-26: $0.50 per GB of logs ingested, and $0.30 a month per custom
metric for an account's first 10,000 ($0.10 from 10,001 to 250,000). Your
region, your account's other metrics and the free tier change them; check
the pricing page for your own.

### Lines per exchange

Each row is an exchange as the gateway's engine records and reports it, at
the default conformance level `none` unless it says otherwise. "Before"
counts the lines of the 18 metrics the gateway wrote before its monitoring
additions (on an exchange: `Exchange`, `ExchangeLatency`, `BackendCall`,
`BackendLatency`, `BackendError`, `LegOutcome`, and `LegError` for a leg that
failed or was unreachable); "after" adds the
additions: `ExchangeRefused`,
`ExchangeRefusedByRule`, `ExchangeStageLatency`, `RequestBytes` and
`ResponseBytes`; `TokenLatency` and `TokenAcquisition` for the
Authorization Framework token the exchange acquires; and `ValidatorLatency`
and `ConformanceFinding` for each `$validate` call and finding.

| Exchange | Lines before | Lines after |
|---|---|---|
| A leg from the network, answered: CRD order-select, DTR questionnaire package, eligibility | 4 | 14 |
| A leg from the network, answered: PAS submit or inquiry | 4 | 15 |
| A CRD leg from the network, answered at `observe` (three `$validate` calls, no finding) | 4 | 18 |
| A leg from the network refused by your gateway before it is dispatched | 2 | 8 |
| A leg from the network refused by your gateway at `strict`, on one finding | 2 | 13 |
| A leg from the network your own system answers with a 4xx | 4 | 14 |
| A leg from the network your own system answers with a 5xx, or does not answer in time | 5 | 15 |
| A call from your own system, answered | 4 | 10 |
| A call from your own system refused by your gateway | 2 | 6 to 7 |
| A call from your own system refused by the Hub | 5 | 13 |
| A call from your own system that fails at the recipient | 5 | 11 |

Each `$validate` call and each finding adds one line wherever it happens. A
leg that waits on a check also adds its `validate` stage line: a conformance
check at `observe`, `structural` or `strict`, and, at every level, module 2's
certification of a message it translates between IG lines
(`TRANSLATE_IG_LINES`). From v1.12.0 a PAS 2.1 or 2.2 request Bundle read against
its profile is still one `$validate` call, but the payload a `validate.result` line
describes is the copy the gateway sent its validator: the Bundle without that
profile, each Claim declaring its Claim profile ([CONFIGURATION.md](CONFIGURATION.md),
`TRANSLATE_IG_LINES`), and its `detail` is the validator's answer on that copy alone: it does
not include the request Bundle profile's own constraints, which the gateway reads itself and
the check's finding includes. Module 2 also validates a DTR answer to learn its IG
line, at every level, with no stage line of its own. With module 2 off, a leg
at `none` makes no `$validate` call. Some lines are written on a timer whatever the
traffic, and are not in these counts: `ActiveRequests`, two a minute, and the
store pool, validator lane and operational gauges.

### Per 1,000 exchanges

For two mixes, which are examples: apply your own.

- **A payer's gateway**, answering the network: 40% CRD order-select, 15% DTR
  questionnaire package, 15% PAS submit, 10% PAS inquiry, 15% eligibility,
  2% refused by the gateway, and 1% each of the payer's system answering
  with a 4xx, with a 5xx, and not in time.
- **A provider's gateway**, calling the Hub for its own system: 96%
  answered, and 1% each refused by the gateway on routing, refused by it for
  want of client credentials, refused by the Hub, and failed at the
  recipient.

| Gateway | Lines before | Lines after | Bytes after | Ingestion after, per 1,000 exchanges | Per million exchanges, before → after |
|---|---|---|---|---|---|
| Payer's | 3,980 | 14,150 | 4.58 MB | $0.0023 | $0.62 → $2.29 |
| Provider's | 3,980 | 9,970 | 3.07 MB | $0.0015 | $0.61 → $1.53 |

A line's size grows with your namespace, `Env` and `Service` values; these
were measured with `SHN/Preview`, `shn-preview` and a 14-character service
name. A GB is 10^9 bytes here.

### Custom metrics per gateway

A gateway's exchange, conformance and in-flight metrics can reach at most 919
metric-and-dimension combinations: 342 from the exchange record's metrics
before the additions and 577 from the additions (165 `ExchangeRefused`, 11 `ExchangeRefusedByRule`, 110
`ExchangeStageLatency`, 33 each of `RequestBytes` and `ResponseBytes`, 210
`ConformanceFinding`, 12 `ValidatorLatency`, 2 `ActiveRequests` and 1
`ObserveCheckDropped`). At $0.30 each, that cap is $275.70 a month. A
gateway reaches only the combinations its own traffic produces, usually a
small fraction: a payer's gateway answers legs from the network, a
provider's gateway calls for its own system, and most refusal combinations
never occur. The gateway's other metrics (`LegOutcome`, `StoreError`, the
store pool, audit, validator lane, certificate, ingress key, registrar feed
and token metrics, and `ListenerCut`, which reaches at most six: the five
bounds on the main listener and `header` on the observer) add combinations
of their own.

Every dimension value apart from `Service` and `Env`, which are your own, comes from a
closed list, so the combinations a gateway can reach are bounded. `ExchangeRefused` and
`ConformanceFinding` leave out the exchange to keep that bound small, and
`ExchangeRefusedByRule` and `BackendError` are one stream per rule and per gateway, for an
alarm; the OpenTelemetry instruments carry the exchange.
ExchangeRefusedByRule{rule=fidelity} counts fidelity refusals at stage verify; build-stage refusals count in ExchangeRefused and the OTel refusals instrument with shn.refusal.stage=build.
`shn.refusal.stage` is `none` for a refusal on any other rule; no EMF line carries it, so
it adds no combination here.

The gateway's own tests pin the line counts, measured over exchanges driven through its
engine, and the series bounds, so these figures change only with a release that says so.

## Running more than one replica

Each replica reports its own series, told apart by the resource's `service.instance.id`
(stable for a host). Prometheus' OTLP receiver turns it into the `instance` label; a
scrape's `instance` label is the target scraped, one per replica. Sum counters
and histograms across replicas; never average them. Read a gauge per replica. An access
line names no replica: tell replicas apart by the labels your log shipper adds. For
`/internal/stats`, merge replicas with `shn gateway stats`, never by averaging their
quantiles.

## Stopping

A stop takes up to 44 s, or 33 s without `DIAGNOSTIC_SINK_URL`; the final metrics export
is one step of it, after the audit outbox drains and the observe checks flush, within 5 s.
Give the container a 45 s stop timeout: see
[DEPLOYMENT.md](DEPLOYMENT.md#stopping-the-gateway).

## Troubleshooting

- **No metrics arrive.** Read `telemetry-config` and `telemetry-export` in
  `/internal/checks`, and the boot log: a setting that is not valid is named there (never
  its value) with its fix. An `OTEL_*` setting the gateway does not read is named in one
  boot warning and ignored, and so are the pull settings when `OTEL_METRICS_EXPORTER`
  does not name `prometheus`.
- **A configuration file exports nothing.** A file that cannot be read or does not
  parse exports nothing, and `telemetry-config` reports it as `OTEL_CONFIG_FILE`,
  without quoting the file. Each setting the gateway does not support, or finds not
  valid, is named by its place in the file (for example
  `meter_provider.readers[0].periodic.exporter.otlp_http.endpoint`).
- **`telemetry-export` fails with `credential-rejected`.** The vendor answered `401` or
  `403` (over gRPC, `Unauthenticated` or `PermissionDenied`): check the headers. Its
  `hint` carries the status. With `unreachable`, the export failed to connect, in TLS
  (a client certificate refused in the handshake included) or in time, or a gRPC
  endpoint answered `Unavailable`, `DeadlineExceeded` or `Canceled`.
- **`telemetry-export` fails with `http-status` and a `hint` naming `redirect-refused`.**
  The OTLP endpoint answered a redirect (the `hint` carries its status), which the
  exporter does not follow. Set the endpoint to its final address, the one that answers
  itself.
- **A plain `http://` endpoint on another host.** It is allowed, and logged at boot:
  metrics then cross the network unencrypted.
- **The pull listener serves nothing.** Off loopback it needs TLS and a scrape credential,
  and a bind that fails (the port taken) turns pull off; `telemetry-config` says which.
  A scrape without the bearer token, or with another, gets `401`.
- **Datadog or New Relic.** Both prefer delta temporality: for Datadog set
  `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE=delta`; the New Relic recipe turns
  cumulative into delta itself.
- **A gauge is missing, not 0.** The operational gauges leave out a value the gateway does
  not know (a server certificate whose endpoint has not answered for 24 hours, the ingress key
  without `SHN_STORE_DATABASE_URL`) rather than report 0. Alert on absence where it matters.

## Examples to start from

`deploy/monitoring/` holds a Grafana dashboard, Prometheus alert rules with their
promtool unit tests, a compose example that runs Prometheus (with its OTLP receiver and
the rules) and Grafana (with the dashboard), and OpenTelemetry Collector recipes for
Datadog, Splunk, New Relic, Azure Monitor, AWS (on the contrib Collector; ADOT carries the same components) and
Grafana Cloud. Its
[README](../deploy/monitoring/README.md) lists every file, rule and threshold. They are
examples outside the stability promise: copy and adapt them. The metric names and
attributes they read are inside it. Each recipe is checked to validate and to forward the
gateway's metrics, in its vendor's format, to a local stand-in; delivery to the vendor
itself is not tested.

The audit-drop alert and the dashboard's audit panels stay silent in v1.0.0, because
nothing appends to the audit outbox yet: their silence means not measured, not no drops.
