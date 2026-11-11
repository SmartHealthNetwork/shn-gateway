# Stability and versioning

## Versioning policy

`shn-gateway` follows semantic versioning. Before v1.0.0 the module is
**pre-1.0 (0.x)**:

- **MINOR** versions (0.x.0 → 0.(x+1).0) may carry breaking changes. Each
  breaking change is called out in the release notes and in
  [`CHANGELOG.md`](CHANGELOG.md).
- **PATCH** versions (0.x.y → 0.x.(y+1)) contain backwards-compatible fixes
  only.

From v1.0.0, semantic versioning applies to the binary contract described
below. The promise binds from the v1.0.0 release: until then, a release may
still reshape any of it.

Each item of the contract is in one of three tiers, which its class sets (How
each item is classified, below):

- **Full** (`full`): only a major release may remove or rename the item, change
  what it means, or narrow its closed set of values.
- **Evolving** (`evolving`): inside the contract, on the full terms, except that
  a minor release may also remove or rename the item, change what it means (its
  default included), or narrow its closed set of values, each with a
  [`CHANGELOG.md`](CHANGELOG.md) note. When a minor release changes an
  evolving setting's default, the gateway logs it at boot in that release,
  naming the setting and its old and new default, so an operator who does not
  read the notes still sees it. The observer event feed and the access line are
  evolving surfaces: every item on them that is inside the promise is evolving,
  whatever its class, and an item outside the promise stays outside. An
  evolving class graduates to the full terms when the tiers table (Tiers at
  v1.0, below) says. When an evolving surface graduates, as its own section
  says, each of its items takes its class's tier.
- **Outside the promise** (`outside`): `test tooling`, `unsupported` and
  `SHN-own` items.

At v1.0 the full tier holds the network's own surface and guarantees
(authentication, authority on each leg, consent, routing and addressing,
replay, audit, message integrity, and the fidelity of the gateway's own edits),
discovery, and the Prior Authorization workstream's CRD, DTR and PAS
pass-through and routing. Eligibility, patient-authored DTR, CDex documentation
exchange, the Federated Query Services workstream and modules 1, 2 and 3 are
evolving.

- A **MAJOR** version is the only release that may break the contract: remove
  or rename a full item, change what a full item means, or narrow a full
  item's closed set of values.
- A **MINOR** version may add items, and values to a closed set, and may change
  an evolving item on the evolving terms above. Decode what the gateway sends
  tolerantly: ignore a field, an event kind or a value you do not know.
- A **PATCH** version contains backwards-compatible fixes only.
- **One recorded exception, v1.1.0:** PAS pass-through is full, and v1.1.0, a
  minor release, adds a registered edit to it by default: the provider's gateway
  re-keys the `payer-url` of each Task in a PAS answer it relays to its own
  system to its documentation return path for the payer that answered (E-10,
  `pas-pend-return-rekey`; participant protocol §7b.1a), when that payer's
  registry entry lists `cdex-attachment-submit`; any other payer's `payer-url` is
  carried as sent. It is made even where
  a signature covers the `payer-url`, then the one edit the gateway made to
  signed content, and the answer names it in `X-SHN-Edited`. It re-keys addressing at
  a participant boundary so that documentation the provider sends stays on the
  network, and nothing used it in production when it shipped. The exception is
  this edit only: it lets no minor release add another edit to a full item, or
  make another edit to signed content. (v1.5.0 records a second exception of its
  own, for one other edit only; see the next entry.)
- **A second recorded exception, v1.5.0:** signed content travelling untouched is
  full, and v1.5.0, a minor release, makes a second edit to signed content. On
  the payer's attachment-request leg (`cdex-attachment-request`, evolving), the
  provider's gateway re-keys the one `payer-url` of the payer's Task, on the
  leg's request only, to its documentation return path for the payer that sent
  it (E-11, `cdex-task-return-rekey`), even where a signature covers the
  `payer-url`, and the forward to the provider's system names it in
  `X-SHN-Edited`. It keeps a documentation return address on the network, as
  E-10 does, so that a Task cannot direct the provider's documentation off the
  network, and no participant used the leg when it shipped. The exception is
  this edit only, on this leg's request only. With it, E-10 and E-11 were the
  only edits the gateway made to signed content, each only at a `payer-url`;
  neither exception lets a minor release add another edit to signed content, or
  make either edit anywhere else. (v1.7.0 records a third exception of its own,
  for two other edits only; see the next entry.)
- **A third recorded exception, v1.7.0:** DTR and PAS pass-through and signed
  content travelling untouched are full. Two registered edits, made in minor
  releases, change one address only: the `$next-question` address an adaptive
  questionnaire's `sdc-questionnaire-questionnaireAdaptive` extension names.
  - **E-12 (`dtr-adaptive-address-rekey`):** on the `dtr-questionnaire-fetch`
    answers a provider's gateway relays to its own DTR client, the provider's
    gateway re-keys that address to its own per-payer address, so that the
    client's `$next-question` rounds stay on the network. It is made from
    v1.8.0, and only toward a payer's gateway that declares it restores the
    address (the `v1opsub` request frame), as a payer's gateway does from
    v1.8.0. v1.7.0 does not make it. **Upgrade note:** E-12 has no opt-out.
    A provider whose own proxy, WAF or load balancer admits only the routes
    earlier releases served must admit the three round routes (Endpoints)
    at its ingress base before upgrading to v1.8.0, or every round its DTR
    client sends is blocked there.
  - **E-13 (`dtr-adaptive-address-restore`):** on the requests a payer's gateway
    forwards to its own system on `dtr-questionnaire-fetch` (the package and
    `$next-question`), `cdex-attachment-submit`, `pas-claim`,
    `pas-claim-update` and `pas-claim-inquire`, the payer's gateway restores
    the address its own system wrote. From v1.7.0 it is made only on an
    address a provider's gateway re-keyed, whose tag this payer's gateway
    issued to that provider and verifies. Until a provider's gateway makes
    E-12, a request carries no such address.
  - **Signed content:** each edit is made even where a signature covers the
    address, so that signature no longer verifies in the copy the receiving
    system gets. The edit is named in `X-SHN-Edited`, with the signature added
    when one covered the address.
  - **Why it was granted:** when it was granted, no partner's DTR client
    fetched through a gateway on preview (a 60-day read), production carried
    no live traffic, only occasional tests, and adaptive DTR had never
    completed through the network.

  The exception is these two edits only, at this address only, on these legs
  only. With it, E-10, E-11, E-12 and E-13 are the only edits the gateway makes
  to signed content, each only at its own address. No exception lets a minor
  release add another edit to a full item or to signed content, or make any
  of these four anywhere else. (v1.10.0 records a fourth exception of its own,
  which makes no edit; see the next entry.)
- **A fourth recorded exception, v1.10.0:** CRD, DTR and PAS pass-through are
  full, and v1.10.0, a minor release, changes one thing in them: the media type
  the provider's gateway writes to its EHR on a CRD, DTR or PAS answer it
  relays.
  - **What changes:** the provider's gateway writes a questionnaire package, a
    `$next-question` answer, and a PAS answer to a claim, an amendment or an
    inquiry with the media type the payer's system stated, whatever it is,
    where earlier releases wrote `application/fhir+json`. A CDS Hooks answer
    stated `application/fhir+json` is written as stated when the payer's
    gateway advertises the `v1opsub` request frame, as a payer's gateway does
    from v1.8.0, and `application/json` from any other payer, as before. An
    answer that states no type is written as before: `application/fhir+json`,
    or `application/json` for a CDS Hooks answer. One no header can carry (it
    does not parse as a media type, is over 256 bytes or holds a control
    character) is written the same way; earlier releases wrote such a CDS
    Hooks type as stated when it parsed and was not
    `application/fhir+json`. The answer's body, status and other
    headers are carried as before.
  - **Signed content:** n/a. This exception changes no byte of the answer, so
    a signature in it verifies as it did before.
  - **Why it was granted:** when it was granted, production carried no live
    traffic, only tests, and the change follows the participant's own type, as
    a direct integration would.
  - **Upgrade note:** an EHR that dispatches on the media type must accept the
    payer's stated type before its provider's gateway upgrades to v1.10.0,
    including the payer's own parameters (for example `;charset=UTF-8`): on a
    DTR or PAS answer, a type other than `application/fhir+json` (for example
    `application/json`); on a CDS Hooks answer, `application/fhir+json` from a
    payer whose gateway advertises `v1opsub`.

  The exception is this media type only, on these answers only, among them a
  DTR answer module 2 carries into the provider's DTR line (E-08): no other
  full item changes, and it lets no minor release change another. (v1.10.0
  records a fifth exception of its own, which makes no edit; see the next
  entry.)
- **A fifth recorded exception, v1.10.0:** CRD pass-through is full, and so
  are the payer client's settings (`PAYER_DAVINCI_TOKEN_URL` and
  `PAYER_DAVINCI_CLIENT_*`), with which earlier releases authenticated every
  call a payer's gateway made to the payer's own system, the CDS service
  listing and each CRD hook included: with the SMART Backend Services access
  token, or with no credential when no token endpoint was set. v1.10.0, a
  minor release, changes how those CDS calls authenticate.
  - **What changes:** on a payer's gateway whose payer client has a signing
    key (`PAYER_DAVINCI_CLIENT_KEY` and `PAYER_DAVINCI_CLIENT_ALG`), the CDS
    service listing read and each CRD hook call carry a CDS Hooks client JWT
    signed with that key, in place of the access token (or of no credential),
    and the gateway serves the key's public JWK Set at
    `GET /cds-client/jwks.json`. A payer client with no signing key, and DTR,
    PAS and eligibility on every payer, authenticate as before. Only the
    `Authorization` header the gateway sets on those calls changes; no byte of
    a request's or an answer's body does.
  - **Signed content:** n/a. This exception changes no byte of a CDS request's
    or answer's body, so a signature in one verifies as it did before.
  - **Why it was granted:** CDS Hooks 2.0, which Da Vinci CRD adopts, defines
    a JWT the client signs as the one way a CDS client authenticates, and a
    payer whose CDS service requires it answered the access token `401`. When
    it was granted, the one hosted payer with a signing key was a payer whose
    CDS service requires the client JWT, and production carried no live
    traffic, only tests.
  - **Upgrade note:** a payer whose payer client has a signing key and whose
    CDS service takes only the access token sets
    `PAYER_DAVINCI_CDS_AUTH=access-token` before its gateway upgrades to
    v1.10.0 (an earlier gateway ignores the setting, and SHN Cloud accepts
    that value on an earlier release); one whose CDS service verifies the
    client JWT registers the
    issuer and the JWK Set URL with it (CONFIGURATION.md, "CDS Hooks client
    authentication").

  The exception is this credential only, on these calls only, on a payer
  client with a signing key: no other full item changes, and it lets no minor
  release change another.
- A full item is deprecated before it is removed: [`CHANGELOG.md`](CHANGELOG.md)
  announces it, and the gateway warns at boot for at least one minor release
  before a major release removes it. An evolving item's removal or change is
  announced in [`CHANGELOG.md`](CHANGELOG.md) with the minor release that
  makes it.
- A setting, full or evolving, goes through the ignored or removed settings
  tables (Settings, below), so a gateway that still sets it is told, never
  silently changed.

v0.39.0 shipped a breaking change under an evolving clause, announced but
unguarded. The ignored and removed settings tables, and SHN's own tests that
hold the settings, endpoints, observer feed, access line and `shn` CLI tables
below to the source in both directions, guard against that: an item cannot be
removed or renamed without its table changing with it.

The `shn` CLI is versioned with `shn-sdk`, on the same terms from `shn-sdk`
v1.0.0, except that a deprecated command or flag warns on standard error, each
time it is used, for at least one minor release before a major release removes
it (The `shn` CLI, below).

A published version tag is **never re-tagged** with different content. The Go
module proxy caches a tag's tree permanently; always bump to a new version
rather than moving an existing tag.

This gateway requires `shn-sdk` — see `go.mod` for the pinned version.

## What the promise covers

Partners run the gateway as a compiled binary or the published container
image, and use the `shn` CLI, which is published with `shn-sdk`. From v1.0.0
the stability promise covers the gateway's **binary contract**:

- its configuration settings;
- its HTTP endpoints, including the observer event feed, `/health` and
  `/internal/checks`;
- its access line and its metrics, EMF and OpenTelemetry;
- the Hub/participant protocol, under the protocol's own versioning;
- the container image;

and, from `shn-sdk` v1.0.0, the `shn` CLI's onboarding commands.

### How each item is classified

Each surface of the binary contract has a section of its own below, opening
with a table of the surface's items. The table has three columns:

- **Item**: the name exactly as the gateway spells it (a setting, an
  endpoint's method and path, an event kind, a JSON field, a metric, a header,
  an image property), one item per row; a field of a nested object or of each
  entry of a list is written `object.field` or `list[].field`. A row names
  one value of an item (`ROLE=phg`, `results[].failure.code=unreachable`)
  where that value's class differs from the item's, or where the table lists
  a closed set of values one by one.
- **Class**: one of the classes below.
- **Owner**: `core` for a core item; otherwise the workstream, module or
  component the item belongs to.

The classes are:

- `core`: the network's own surface.
- `PA workstream` (Prior Authorization), `FQS workstream` (Federated Query
  Services): a workstream's own surface, documented under that workstream.
- `PA workstream, evolving`: the Prior Authorization workstream's eligibility,
  patient-authored DTR and CDex documentation exchange items (the CDex legs,
  from v1.1.0 `cdex-attachment-submit` and from v1.5.0
  `cdex-attachment-request`, and their ingress, return paths and size limits),
  owned by `PA workstream`, which are evolving while its CRD, DTR
  and PAS items are full.
- `module 1` (building messages on a participant's behalf), `module 2`
  (bridging and translating between IG lines), `module 3` (opt-in conformance
  checks): an opt-in module's surface, in effect only where the participant
  turns that module on.
- `monitoring`: the gateway's monitoring surface: the OpenTelemetry export
  settings, the checks that report on them, the OpenTelemetry instruments and
  their attributes, the metrics and access line fields added with them, the
  access line's `readsFailed` (from v1.10.0), whose scope grows as reads adopt
  it, the `/internal/stats` endpoint and `shn gateway stats`, the `shn` CLI's
  command that reads it. Owned by `core`, evolving at v1.0.
- `test tooling`: for test deployments; outside the promise. A released
  gateway reads some of it only to refuse to start with it.
- `unsupported`: not acted on (the gateway or the CLI reads it only to warn or
  to refuse, it has no effect, or it is only a convention); outside the
  promise.
- `SHN-own`: set or used only by SHN's own components and deployments (the
  PHG, SHN Cloud); a partner's gateway neither sets it nor relies on it.

Each class has one tier (Versioning policy, above), which the next table lists
with the trigger on which an evolving class graduates to the full terms. An item
on the observer event feed or the access line that is inside the promise is
evolving whatever its class; one outside the promise stays outside.

The sections that follow each table describe the surface's behavior in
detail, with the release that introduced or changed it. A section that
describes the behavior of an item of an evolving class opens with an **Evolving
(v1.0)** line naming it. The subsections of the observer event feed and the
access line need no such line for the surface itself, which their surface's
own section states; one carries it for an item of an evolving class.

### Tiers at v1.0

| Class | Tier | Graduates when |
|---|---|---|
| `core` | full | Full from v1.0.0. The observer event feed and the access line are evolving surfaces, each graduating when its own section says. |
| `PA workstream` | full | Full from v1.0.0: CRD, DTR and PAS pass-through and routing. |
| `PA workstream, evolving` | evolving | Eligibility graduates when the responder default ships (a request is carried to the payer's own endpoint unless the payer declares that its system cannot answer it) and a partner payer's own endpoint answers it. Patient-authored DTR graduates when a participant other than SHN's PHG answers it. CDex documentation exchange graduates once it has shipped and partners use it. |
| `FQS workstream` | evolving | Graduates when the CDex carry path ships and a partner facility answers through it. |
| `module 1` | evolving | Graduates when module 1 sends what it builds through the gateway's own ingress and a partner declares it (`ORIGINATION_PROFILE`) in production. |
| `module 2` | evolving | Graduates when a certified translation for a pair of IG lines is in partner use. |
| `module 3` | evolving | Graduates when the rule set of `structural` and `strict` is frozen and a partner runs one of them in production. |
| `monitoring` | evolving | Graduates in a minor release that declares it. |
| `test tooling` | outside | Outside the promise; does not graduate. |
| `unsupported` | outside | Outside the promise; does not graduate. |
| `SHN-own` | outside | Outside the promise; does not graduate. |

## Go APIs are SHN-internal

The Go packages of this module (`engine`, `app`, `connectors/...`, `observer`,
`diagnostics`, `scenariodriver`, `fhirseed` and the rest) and of `shn-sdk` carry
**no compatibility promise**. A release may change or remove them, and such a
change is not a breaking change of the gateway. After v1.0.0, within a major,
such a change comes in a minor release, never a patch, and that release's notes
name each incompatible change in their "Go API" section. The source stays public
so that what the binary does can be read; to build on SHN, configure the published
image ([`docs/CONFIGURATION.md`](docs/CONFIGURATION.md)) and connect your
systems to it over HTTP ([`docs/INTEGRATION.md`](docs/INTEGRATION.md)).

These test doubles and test-only seams exist only in a build tagged
`shn_testdouble`: the hermetic line validator, the `fhirServer` read's trust
seam, the ingress auth bypass, `relay.ForTest` and `relay.BytesForTest`, and the
module-absent opt-outs.
Other exported `*ForTest` helpers stay in every build. An
untagged build, as every release is, neither links nor exports them, and the
module's own tests need `-tags=shn_testdouble`.

Release notes may still name a Go API change, as information. What earlier
releases said about Go packages partners could depend on is kept in
[`CHANGELOG.md`](CHANGELOG.md), "Go API notes before v1.0.0".

## Settings

The gateway is configured through environment variables, documented in
[`docs/CONFIGURATION.md`](docs/CONFIGURATION.md). The table lists every setting
the released binary reads; the settings it no longer acts on, which it reads
only to warn or to refuse to start, or ignores; and `SSL_CERT_DIR` and
`SSL_CERT_FILE`, which the Go runtime reads for the trust roots of the
gateway's outbound TLS and [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md)
names.

Environment variables read by the Go runtime or third-party libraries rather
than by the gateway itself (for example `PG*` for the Postgres driver,
`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY`, `SSL_CERT_FILE`/`SSL_CERT_DIR`) are
honoured as those libraries define them. They are outside SHN's promise,
except where [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md) names one.
`HTTP_PROXY`/`HTTPS_PROXY`/`NO_PROXY` are honoured on the calls whose client
uses Go's default transport; [`docs/CONFIGURATION.md`](docs/CONFIGURATION.md)
names the calls that use no proxy. A library's setting the gateway makes a
no-op is listed `unsupported` below, and the gateway names it in a boot
warning: `OTEL_GO_X_DEPRECATED_RUNTIME_METRICS`, read by the OpenTelemetry Go
runtime instrumentation, whose deprecated metrics the gateway drops from every
export (the gateway's table of these settings is `noEffectSettings` in
`gateway/app`).

| Item | Class | Owner |
|---|---|---|
| `ROLE` | core | core |
| `ROLE=phg` | SHN-own | PHG |
| `PORT` | core | core |
| `HOST` | core | core |
| `SHN_SECRETS` | core | core |
| `SHN_DISCOVERY_URL` | core | core |
| `AUTHZ_URL` | core | core |
| `HUB_URL` | core | core |
| `CONSENT_URL` | core | core |
| `REGISTRAR_URL` | core | core |
| `AUTHZ_PUBKEY_URL` | core | core |
| `HUB_TRANSPORT_KEY_URL` | core | core |
| `TLS_CERT_FILE` | core | core |
| `TLS_KEY_FILE` | core | core |
| `SHN_STORE_DATABASE_URL` | core | core |
| `SHN_STORE_MAX_CONNS` | core | core |
| `EXCHANGE_TTL` | core | core |
| `CHECKS_TOKEN` | core | core |
| `OBSERVER_ADDR` | core | core |
| `METRICS_SERVICE` | core | core |
| `METRICS_NAMESPACE` | core | core |
| `METRICS_ENV` | core | core |
| `OTEL_METRICS_EXPORTER` | monitoring | core |
| `OTEL_SDK_DISABLED` | monitoring | core |
| `OTEL_CONFIG_FILE` | monitoring | core |
| `OTEL_SERVICE_NAME` | monitoring | core |
| `OTEL_RESOURCE_ATTRIBUTES` | monitoring | core |
| `OTEL_METRIC_EXPORT_INTERVAL` | monitoring | core |
| `OTEL_METRIC_EXPORT_TIMEOUT` | monitoring | core |
| `OTEL_EXPORTER_OTLP_METRICS_TEMPORALITY_PREFERENCE` | monitoring | core |
| `OTEL_EXPORTER_OTLP_METRICS_DEFAULT_HISTOGRAM_AGGREGATION` | monitoring | core |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | monitoring | core |
| `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` | monitoring | core |
| `OTEL_EXPORTER_OTLP_PROTOCOL` | monitoring | core |
| `OTEL_EXPORTER_OTLP_METRICS_PROTOCOL` | monitoring | core |
| `OTEL_EXPORTER_OTLP_HEADERS` | monitoring | core |
| `OTEL_EXPORTER_OTLP_METRICS_HEADERS` | monitoring | core |
| `OTEL_EXPORTER_OTLP_TIMEOUT` | monitoring | core |
| `OTEL_EXPORTER_OTLP_METRICS_TIMEOUT` | monitoring | core |
| `OTEL_EXPORTER_OTLP_COMPRESSION` | monitoring | core |
| `OTEL_EXPORTER_OTLP_METRICS_COMPRESSION` | monitoring | core |
| `OTEL_EXPORTER_OTLP_CERTIFICATE` | monitoring | core |
| `OTEL_EXPORTER_OTLP_METRICS_CERTIFICATE` | monitoring | core |
| `OTEL_EXPORTER_OTLP_CLIENT_KEY` | monitoring | core |
| `OTEL_EXPORTER_OTLP_METRICS_CLIENT_KEY` | monitoring | core |
| `OTEL_EXPORTER_OTLP_CLIENT_CERTIFICATE` | monitoring | core |
| `OTEL_EXPORTER_OTLP_METRICS_CLIENT_CERTIFICATE` | monitoring | core |
| `OTEL_EXPORTER_PROMETHEUS_HOST` | monitoring | core |
| `OTEL_EXPORTER_PROMETHEUS_PORT` | monitoring | core |
| `METRICS_PULL_TOKEN` | monitoring | core |
| `METRICS_PULL_CLIENT_CA_FILE` | monitoring | core |
| `METRICS_PULL_TLS_CERT_FILE` | monitoring | core |
| `METRICS_PULL_TLS_KEY_FILE` | monitoring | core |
| `OTEL_GO_X_DEPRECATED_RUNTIME_METRICS` | unsupported | core |
| `SHN_CONTRACT_VERSIONS` | core | core |
| `REQUIRE_SHARED_LINE` | core | core |
| `FHIR_DATA_URL` | core | core |
| `FHIR_TOKEN_URL` | core | core |
| `FHIR_CLIENT_ID` | core | core |
| `FHIR_CLIENT_KEY` | core | core |
| `FHIR_CLIENT_ALG` | core | core |
| `FHIR_CLIENT_SCOPE` | core | core |
| `FHIR_CLIENT_KID` | core | core |
| `FHIR_CLIENT_SECRET` | core | core |
| `FHIR_VALIDATE_URL` | core | core |
| `FHIR_VALIDATE_URL_2_1` | core | core |
| `FHIR_VALIDATE_URL_2_2` | core | core |
| `FHIR_DEFAULT_VALIDATOR_LANES` | core | core |
| `FHIR_CERTIFY_URL_2_1` | module 3 | module 3 |
| `FHIR_CERTIFY_URL_2_2` | module 3 | module 3 |
| `CONFORMANCE_ENFORCEMENT` | core | core |
| `REQUIRE_KNOWN_MEMBERS` | module 3 | module 3 |
| `CONFORMANCE_TOLERATE_PEERS` | module 3 | module 3 |
| `SHN_ACCEPT_UNKNOWN_MEMBERS` | unsupported | module 3 |
| `TRANSLATE_IG_LINES` | module 2 | module 2 |
| `ENRICH_NATIVE_REQUESTS` | module 1 | module 1 |
| `NPI` | module 1 | module 1 |
| `ORIGINATION_PROFILE` | module 1 | module 1 |
| `PROVIDER_DTR_NATIVE` | module 1 | module 1 |
| `PROVIDER_DTR_POPULATE_URL` | module 1 | module 1 |
| `PROVIDER_DTR_POPULATE_TOKEN_URL` | module 1 | module 1 |
| `PROVIDER_DTR_POPULATE_CLIENT_ID` | module 1 | module 1 |
| `PROVIDER_DTR_POPULATE_CLIENT_KEY` | module 1 | module 1 |
| `PROVIDER_DTR_POPULATE_CLIENT_ALG` | module 1 | module 1 |
| `PROVIDER_DTR_POPULATE_SCOPE` | module 1 | module 1 |
| `PROVIDER_DTR_POPULATE_CLIENT_KID` | module 1 | module 1 |
| `PROVIDER_DTR_POPULATE_CLIENT_SECRET` | module 1 | module 1 |
| `PROVIDER_CDEX_TASK_URL` | PA workstream, evolving | PA workstream |
| `PROVIDER_CDEX_TOKEN_URL` | PA workstream, evolving | PA workstream |
| `PROVIDER_CDEX_CLIENT_ID` | PA workstream, evolving | PA workstream |
| `PROVIDER_CDEX_CLIENT_KEY` | PA workstream, evolving | PA workstream |
| `PROVIDER_CDEX_CLIENT_ALG` | PA workstream, evolving | PA workstream |
| `PROVIDER_CDEX_SCOPE` | PA workstream, evolving | PA workstream |
| `PROVIDER_CDEX_CLIENT_KID` | PA workstream, evolving | PA workstream |
| `PROVIDER_CDEX_CLIENT_SECRET` | PA workstream, evolving | PA workstream |
| `PROVIDER_DAVINCI_INGRESS` | core | core |
| `PROVIDER_DAVINCI_INGRESS_BASE_URL` | core | core |
| `INGRESS_CLIENTS_FILE` | core | core |
| `PAYER_DAVINCI_INGRESS` | PA workstream, evolving | PA workstream |
| `PAYER_DAVINCI_INGRESS_BASE_URL` | PA workstream, evolving | PA workstream |
| `CDS_FHIR_SERVER_READ` | PA workstream | PA workstream |
| `CDS_ADVERTISE_HOOKS` | PA workstream | PA workstream |
| `PAYER_DIRECTORY` | PA workstream | PA workstream |
| `PAYER_DAVINCI_BASE_URL` | PA workstream | PA workstream |
| `PAYER_DAVINCI_CDS_BASE_URL` | PA workstream | PA workstream |
| `PAYER_DAVINCI_CDS_AUTH` | PA workstream | PA workstream |
| `PAYER_DAVINCI_CDS_JWT_ISS` | PA workstream | PA workstream |
| `PAYER_DAVINCI_CDS_JWT_JKU` | PA workstream | PA workstream |
| `PAYER_DAVINCI_DTR_BASE_URL` | PA workstream | PA workstream |
| `PAYER_DAVINCI_PAS_BASE_URL` | PA workstream | PA workstream |
| `PAYER_DAVINCI_TOKEN_URL` | PA workstream | PA workstream |
| `PAYER_DAVINCI_CLIENT_ID` | PA workstream | PA workstream |
| `PAYER_DAVINCI_CLIENT_KEY` | PA workstream | PA workstream |
| `PAYER_DAVINCI_CLIENT_ALG` | PA workstream | PA workstream |
| `PAYER_DAVINCI_SCOPE` | PA workstream | PA workstream |
| `PAYER_DAVINCI_CLIENT_KID` | PA workstream | PA workstream |
| `PAYER_DAVINCI_CLIENT_SECRET` | PA workstream | PA workstream |
| `PAYER_DAVINCI_CRD_SERVICE_ID` | PA workstream | PA workstream |
| `PAYER_DAVINCI_DISPATCH_SERVICE_ID` | PA workstream | PA workstream |
| `PAYER_DAVINCI_CONTRACT_VERSIONS` | PA workstream | PA workstream |
| `PAYER_DAVINCI_STRICT_EXTENSIONS` | unsupported | PA workstream |
| `PAYER_DAVINCI_BACKEND_HEADERS` | PA workstream | PA workstream |
| `PAYER_DAVINCI_BACKEND_CORRELATION` | PA workstream | PA workstream |
| `PAYER_DAVINCI_BACKEND_TIMEOUT` | PA workstream | PA workstream |
| `PAYER_DAVINCI_PAYOR_OWN` | PA workstream | PA workstream |
| `PAYER_DAVINCI_PAYOR_BACKEND` | PA workstream | PA workstream |
| `PAYER_ELIGIBILITY_URL` | PA workstream, evolving | PA workstream |
| `ATTACHMENT_CEILING_BYTES` | PA workstream, evolving | PA workstream |
| `ATTACHMENT_INFLIGHT_BYTES` | PA workstream, evolving | PA workstream |
| `ECS_CONTAINER_METADATA_URI_V4` | PA workstream, evolving | PA workstream |
| `PAYER_ATTACHMENT_MAX_BYTES` | PA workstream, evolving | PA workstream |
| `PAYER_DAVINCI_PAS_NATIVE` | unsupported | PA workstream |
| `SCENARIO_CALLERS_FILE` | test tooling | test tooling |
| `SHN_DEMO_EGRESS_NATIVE_LINES` | test tooling | test tooling |
| `SHN_DEMO_EDGE_CAPTURE` | test tooling | test tooling |
| `DIAGNOSTIC_SINK_URL` | test tooling | test tooling |
| `DIAGNOSTIC_SOURCE` | test tooling | test tooling |
| `DIAGNOSTIC_KEY_FILE` | test tooling | test tooling |
| `DIAGNOSTIC_TRACE_KEY_FILE` | test tooling | test tooling |
| `SHN_VERSION` | SHN-own | SHN Cloud |
| `SHN_FAKE_VALIDATOR` | test tooling | test tooling |
| `PATIENT_ACCESS_API` | unsupported | core |
| `AUDIT_URL` | unsupported | core |
| `PHG_URL` | unsupported | core |
| `PAYER_DAVINCI_CRD_COVERAGE_BUNDLE` | unsupported | PA workstream |
| `PAYER_DAVINCI_CRD_HOOK` | unsupported | PA workstream |
| `PAYER_DAVINCI_DISPATCH_HOOK` | unsupported | PA workstream |
| `SSL_CERT_DIR` | core | core |
| `SSL_CERT_FILE` | core | core |

### A payer that keeps no system of record

**Evolving (v1.0):** what this section says of coverage eligibility
(`PA workstream, evolving`); the rest of it is full.

**Additive in v0.58.0.**

- A payer whose own system answers every exchange (`ROLE=payer` with
  `PAYER_DAVINCI_BASE_URL`) may leave `FHIR_DATA_URL` unset. Earlier releases refused
  to boot without it on every role; every other gateway still does. A deployment that
  boots on v0.57.0 boots unchanged.
- Such a gateway binds each member from the request that names it, exactly as it binds
  a member a system of record does not hold, and files its records under that binding.
  It answers coverage eligibility only through `PAYER_ELIGIBILITY_URL`; without it an
  eligibility request is answered `501` "coverage eligibility is not offered by this
  payer", in the shape every refusal the payer's gateway makes itself on that leg takes
  (from v1.10.0 an `OperationOutcome`, before `{"error": …}`).
  `REQUIRE_KNOWN_MEMBERS=true` without a system of record refuses to boot. Patient Access
  returns no explanations of benefit on such a gateway: they are filed under its binding
  of each member, never under the identifier a patient-access token names, as for any
  member a system of record does not hold.

### Origination profiles

**Evolving (v1.0):** origination profiles are module 1's (`module 1`).

**Breaking in v1.0.0.**

- `ORIGINATION_PROFILE` is the participant's declaration that its own system
  cannot speak Da Vinci, so its gateway runs module 1 (the `/scenario/*`
  origination API) for it. Unset, module 1 is off on every role: the gateway
  serves its ingress and relays, mounts no `/scenario` route (each answers
  `404`), and needs no module 1 setting. Before v1.0.0, unset meant `demo` for
  `ROLE=provider`.
- With module 1 off, `SCENARIO_CALLERS_FILE` refuses to boot:
  `gateway: SCENARIO_CALLERS_FILE requires ORIGINATION_PROFILE: with none, module 1 is off and no route serves /scenario`.
  `NPI`, `PROVIDER_DTR_NATIVE`, `PROVIDER_DTR_POPULATE_URL` and
  `PROVIDER_DTR_POPULATE_TOKEN_URL` are unused, and the boot output names each
  one that is set. They are still checked at boot: an invalid value, or an
  incomplete group, refuses boot as before.

**Behavior change in v0.57.0.**

- `ORIGINATION_PROFILE` takes `demo` or `provider-data`; unset meant `demo` for
  `ROLE=provider` until v1.0.0. Any other value refuses to boot, on every role, with
  `gateway: invalid ORIGINATION_PROFILE "<value>" (must be demo|provider-data, or unset)`.
  The value is matched exactly, so `Demo` or ` provider-data` refuses too.
- Earlier releases accepted an unknown value, and a provider's gateway then built
  the requests it originates on no known lane: its PAS request carried a payer
  Organization that no system of record supplied. A deployment that boots on
  v0.56.0 with an accepted value boots unchanged.

### Conformance enforcement levels

**Evolving (v1.0):** the checks each level runs are module 3's (`module 3`) and
evolving. The setting `CONFORMANCE_ENFORCEMENT`, its values (`none`, `observe`,
`structural`, `strict`) and its unset default (`none`) are not: they are full.
So are the network rules, which refuse at every level, and the v0.53.0 carry of
members the system of record does not hold on the CRD, DTR and PAS legs
(below).

`CONFORMANCE_ENFORCEMENT` takes `none` (the default when unset, from v1.0.0),
`observe` (the default when unset before v1.0.0), `structural` or `strict` (see
[docs/CONFIGURATION.md](docs/CONFIGURATION.md)).

**Behavior change in shn-gateway v1.4.0: a Coverage's party may be a `Patient` entry
of its own.** A dependent's parent that a PAS request or answer carries as a `Patient` entry of
its own that only a Coverage's `subscriber` or `policyHolder` names (by its exact `fullUrl`, or
by a relative reference that resolves to it against the Coverage entry's base, never by id
alone) is the Coverage's party, as a parent contained in the Coverage already is.
shn-sdk's `CoverageEntryParties` decides it. It does not check that the Coverage's
`beneficiary` is the member: the checks below do, and refuse another member as before. The
request's check for another patient (`patient.mixed`, the inquiry's member check included) and the answer's
(`patient.answer`) do not read it as a second patient, and the patients a request involves (below)
do not include it. Such a request or answer was refused at `strict` and recorded at `observe` and
`structural`; it is now carried and read at every level. Any other `Patient` entry is read as
before.

**Behavior change in v1.0.0: an unset level is `none`.** A gateway that leaves
`CONFORMANCE_ENFORCEMENT` unset runs no payload conformance check, records no
finding and needs no validator to boot, exactly as at `none`; before v1.0.0 it
ran at `observe`. Payload conformance is the participant's opt-in: set `observe`,
`structural` or `strict` to keep checking. Nothing is newly refused: the network
rules and the check of a payload the gateway itself translated between IG lines
refuse at every level, as before. The boot line names the resolved level and says
when it came from the unset default.

**Behavior change in v0.55.0: an unknown code system is recorded at `structural`.**

- A coding whose code system the validator does not know
  (`Terminology_TX_System_Unknown`) is now recorded at `structural`, like any
  other code system the validator cannot check, when nothing else in the message
  refuses at that level. Earlier releases refused it at `structural`.
- The validator reports this issue as an error only for a system in the HL7 FHIR
  namespace. That includes a misspelled HL7 system URL, which the validator
  cannot tell apart from one it has not loaded, so it too is recorded.
- `strict` still refuses it, and `observe` still records it.
- The validator image built from v0.55.0's or a later `deploy/validator`
  resolves the `version-algorithm` code system on the 2.0 and 2.1 lines, which
  earlier images reported as an unknown code system. An image built before
  v0.55.0 still reports it, and `strict` refuses it; rebuild the image with the
  gateway.
- In v0.55.0, apart from this and the provider's check of a payer's answers
  (below), the four levels behave as in v0.54.0. v0.56.0 adds the check of a
  CDS Hooks answer on the legs a provider originates (below).

**New in v0.54.0** (v0.53.x and earlier refuse to boot on `structural`):

- The new `structural` level runs every check, refuses a message whose structure is
  broken with the status and body `strict` gives it, and records every other
  defect as `observe` does. A FHIR validator issue is classified by the
  validator's message id: only invariants and the validator's recognized issues
  for a code outside its code list (licensed code systems included) are
  recorded, and every other FHIR profile issue, any other terminology issue,
  every fatal issue and an issue it cannot classify refuses. A validator that cannot run is recorded, not
  refused.
- A `Parameters` resource is now sent to the validator inside the operation's
  `resource` parameter, so the validator reads it. A validator that answers that
  it was given no resource (`HAPI-0992`) is treated as unavailable at every
  level: `strict` refuses it with `500` (it was a `422` naming the validator's
  text before), `observe` and `structural` record it as unavailable and relay.
- Unset, `none`, `observe` and `strict` are otherwise unchanged.

**Behavior change in v0.53.0** (v0.52.0 refuses to boot on `observe`):

- An unset `CONFORMANCE_ENFORCEMENT` now means `observe`, not `none`. Like
  v0.52.0's `none`, `observe` runs every check and records each defect, but
  refuses nothing a check finds: the content defects earlier releases' `none`
  refused at every level (below) are now relayed with a finding. A gateway still
  needs its validator to boot, as before. `engine.Config.ConformanceEnforcement`'s
  zero value is still `strict`: only the published binary's environment loader
  maps an unset value to `observe`.
- `none` now runs no payload conformance check. It no longer validates, applies
  the CDS Hooks response rules or the content checks, gathers certification
  evidence or validates the resources a CDS Hooks answer embeds, and it records
  no finding. A gateway that sets `none` explicitly and relied on it for findings
  sees none; remove the setting, or set `observe`, to keep them.
- The new `observe` level runs every check, records each defect as a finding (the
  `conformance:` log line and the `conformance.observed` event) and carries the
  message as sent, apart from the gateway's registered edits (the callback
  removed, prefetch and coverage obtained where the participant has opted in to
  them, and payer identity mapping; see the participant protocol §7a.4).
  Operators who want findings without refusals set `observe`.
- A content defect (a request's or answer's own shape or internal consistency, a
  prefetch value the system of record cannot supply, an answer this gateway
  cannot read) refuses only at `strict`, with the status and body it has always
  had. Below `strict` the message is carried or relayed as sent, apart from the
  gateway's registered edits, and a PAS or inquiry answer relayed unread writes
  nothing to the local record.
- Network rules refuse at every level: authentication, authority (including a
  token presented with a request other than the one it was issued for), consent,
  the patient binding (as changed below), routing and addressing, replay, a
  repeated member name in any body, the contract line stamped on an answer's
  frame, and the check of a payload this gateway translated between IG lines.
- `strict`'s conformance refusals keep their statuses and bodies. `strict` now
  also records a finding of kind `content` when it refuses a message for a
  content defect, and a finding carries an optional `verdict` field
  (`unavailable` when a check could not finish; absent for an invalid result).

**Behavior change in v0.53.0: members the system of record does not hold are
carried by default.**

- On the CRD, DTR and PAS legs such a subject is now carried, identified by the
  member id and the Patient the request carries for it, on the provider ingress
  and the payer inbound alike. Send the same Patient, unchanged, on every leg of
  one exchange: for a member the payer does not hold, an amendment carrying a
  different Patient, or none, finds no pended authorization and is refused
  `409`, and an inquiry's decision is not recorded (observer event
  `pend.other-subject`). Earlier releases refused it with `unknown member`.
  `REQUIRE_KNOWN_MEMBERS=true` opts in to that refusal; any value other than
  `true` or `false` refuses to boot.
- **No longer read (v1.0.0):** a gateway that sets `SHN_ACCEPT_UNKNOWN_MEMBERS`
  logs a warning and starts. Beside `REQUIRE_KNOWN_MEMBERS=true` it no longer
  refuses to boot: `REQUIRE_KNOWN_MEMBERS` alone decides. Earlier releases
  logged a deprecation warning, and refused to boot on that pair.
- A CRD request for a member the system of record does not hold must carry its
  own `coverage` prefetch (otherwise `422`, at every level); history prefetch the
  gateway cannot read for that member is left out, with the reason recorded. With
  `REQUIRE_KNOWN_MEMBERS=true` the member is refused instead.
- The provider's own Patient append on `$questionnaire-package`, which earlier
  releases applied under `SHN_ACCEPT_UNKNOWN_MEMBERS`, is no longer applied: a
  request carrying no Patient is carried as sent. It returns as a participant
  opt-in (`ENRICH_NATIVE_REQUESTS`, below). The other registered edits (callback
  removed, prefetch obtained, coverage obtained, payer identity mapping) are
  unchanged in v0.53.0.
- The receiving gateway no longer compares the patient the leg's token names with
  the patient the request names on the CRD, DTR, PAS and inquiry legs: it handles
  the member the request names as it would directly. A request whose member the
  two sides identify differently now reaches the receiver's own system instead of
  being refused `403 token subject does not match request patient`. Everything
  the payer gateway records about an exchange (the pend ledger, a decision
  ExplanationOfBenefit, the correlation it claims, an inquiry's decision) is filed
  under its own binding of the member the request names, never under the patient
  the token names. Eligibility, federated query and patient-authored DTR keep
  their own check of the token's patient. A token presented with a
  request other than the one it is bound to is refused, as before. When the
  payer's binding is not the token's patient, the payer answers as usual and
  raises the observer event `subject.binding-differs`; the network's audit
  records the exchange under the patient the token names.
- **Mixed releases:** a payer gateway before v0.53.0 still compares the token's
  patient with its own binding, and refuses a member its system of record does not
  hold (`400 unknown member`) unless it sets `SHN_ACCEPT_UNKNOWN_MEMBERS`. A
  request that a v0.53.0 payer would accept can therefore still be refused by an
  older payer: `400 unknown member`, or `403 token subject does not match request
  patient` when the two sides identify the member differently. A provider gateway
  from v0.53.0 also no longer appends its own Patient to a
  `$questionnaire-package` request, so for a member the provider holds and an
  older payer running with `SHN_ACCEPT_UNKNOWN_MEMBERS` does not, that request is
  refused `403 token subject does not match request patient`. Upgrade payer
  gateways before the provider gateways that send to them.

### Networks without default validator lanes

**New in v0.55.0.**

- `FHIR_DEFAULT_VALIDATOR_LANES` accepts unset or `none`; any other value
  refuses to start (see [docs/CONFIGURATION.md](docs/CONFIGURATION.md)).
  - Unset keeps the earlier behavior: the gateway probes the Compose default
    validator names (`shn-validator-2-1`, `shn-validator-2-2`) for a line with no
    address of its own.
  - `none` states that this gateway's network has no such services. The gateway
    creates no default lane and never probes their names.
- With `none`, a CRD, DTR or PAS line other than 2.0 that `SHN_CONTRACT_VERSIONS`
  declares needs its own `FHIR_VALIDATE_URL_<line>`; otherwise the gateway
  refuses to start, naming the key. `FHIR_CERTIFY_URL_<line>` does not satisfy
  that requirement. A line with neither key dials nothing for certification
  evidence (Observational source certification, below). A single-line
  contract's line (`pa.pdex@2.1`) still rides the canonical validator, as it
  does with default lanes.
- SHN Cloud sets `FHIR_DEFAULT_VALIDATOR_LANES=none` for hosted gateways on
  v0.55.0 or later. It is not a tenant setting. A self-hosted gateway that
  leaves it unset keeps the default lanes.
- **Failed probes name their cause.** The `validator_qualification` and
  `certification_lane_qualification` log lines now carry the `host` they dial,
  and a `failed` line also carries a `reason`: `name does not resolve`,
  `name lookup failed`, `connection refused`, `metadata answered HTTP <status>`,
  `metadata is not an R4 CapabilityStatement`, `qualification corpus did not
  pass`, `no answer within the qualification budget`, `qualification stopped`
  or `qualification did not pass`. The line never includes error text or a
  validator's answer. A lane that never answers keeps the cause of its last
  failed attempt, so it still says why.
- **Rolling back.** A gateway before v0.55.0 ignores
  `FHIR_DEFAULT_VALIDATOR_LANES`: it probes the Compose default names again, and
  its failed probes carry no `host` or `reason`.

### Enrichment of native requests

**Evolving (v1.0):** the enrichments are module 1's (`module 1`); carrying a
native request as sent is full.

**Behavior change in v0.54.0: a Da Vinci-native request is carried as sent unless
the participant opts in to enrichment.**

- `ENRICH_NATIVE_REQUESTS` takes `true` or `false`; unset means `false`, and any
  other value refuses to boot. It governs the provider ingress's three enrichments,
  each read from the participant's own system of record: the CDS Hooks prefetch fill,
  the `$questionnaire-package` Coverage append and the `$questionnaire-package`
  Patient append (registered edits E-02, E-04 and E-05).
- By default none of them runs. A CDS Hooks request is sent with only `fhirServer`
  and `fhirAuthorization` removed (from v0.61.0, and with the coverage read
  through that `fhirServer` to route by carried, E-07); a
  `$questionnaire-package` request is sent byte for byte. Earlier releases added the advertised prefetch values and the
  Coverage a request left out without being asked; an EHR that relied on that now
  sends them itself, or its gateway sets `ENRICH_NATIVE_REQUESTS=true`.
- A coverage a request leaves out is still read from the system of record, to
  choose the payer; it is not added to the request. That read is made under the
  system's own Patient id, so a system that names the patient by another id no longer
  refuses such a request by default, on the CDS Hooks and questionnaire-package
  ingress alike (nor, from v0.61.0, under the opt-in: see "A patient the system of
  record names by another id, under the opt-in"). A request whose coverage cannot be found is refused as before.
- With `ENRICH_NATIVE_REQUESTS=true` the three edits behave as in earlier releases,
  and the Patient append no longer depends on the unknown-member setting.
- Requests the gateway builds itself (`ORIGINATION_PROFILE`) are not affected.

### Payer eligibility endpoint

**Evolving (v1.0):** coverage eligibility is `PA workstream, evolving`.

Added in v0.54.0.
- `PAYER_ELIGIBILITY_URL` declares a payer's own coverage-eligibility endpoint. A
  coverage-eligibility request is then carried to it exactly and the payer's answer relayed
  (an error answer as the payer's error). No answer is built from the payer's records in its
  place, whatever the payer's system answers or fails to answer.
- Unset, the default, is unchanged: the gateway answers eligibility from the payer's records.
- On the forwarded path the member is bound by the payer's own records and a token naming
  another patient is not refused, as on the prior-authorization legs. An answer about another
  patient, or naming its patient otherwise than by reference, is relayed below `strict` and
  refused at `strict` (`RulePatientAnswer`); one that cannot be read as a
  `CoverageEligibilityResponse`, or names no patient at all, is refused at `structural` and `strict` (`RuleAnswerShape`); one naming
  the patient by the payer's own Patient id for that member is the same patient. A failed read
  of the payer's records for that comparison refuses only at `strict`.
- The request, and the payer's answer, are `$validate`d at the conformance level (no call at
  `none`); the relayed PAS and questionnaire answers are not. A request the level refuses is
  refused before the payer's system is called.

### Identifiers for members the system of record does not hold

**Behavior change in v0.55.0.**

- A member your system of record does not hold is carried, as before. The
  network identifier the gateway gives such a member is now in its own
  namespace, so it can never equal the identifier of a member your system of
  record holds. Earlier releases could give such a member the identifier of a
  member your system of record holds, and so file the exchange, and what the
  payer recorded about it, under that member.
- The bytes a provider and a payer exchange are unchanged. Only the network's
  own identifiers differ: when the provider holds a member and the payer does
  not, the two gateways now identify that person differently. Each gateway
  files its own records under its own identifier; the network's audit records
  the exchange under the identifier the request's authorization names.
- A payer gateway raises `subject.binding-differs` on every exchange about a
  member only one side's system of record holds, since the two identifiers now
  always differ; earlier releases raised it there less often. Expect more of
  these events after the upgrade. An exchange about a member neither side
  holds, with the same Patient on both sides, raises none.
- **Mixed releases:** a payer gateway before v0.53.0 still compares the token's
  patient with its own binding. For a member the provider's system of record
  does not hold, a v0.55.0 provider's identifier no longer equals the one such
  a payer holds for that member, or derives for it under
  `SHN_ACCEPT_UNKNOWN_MEMBERS`, so the request is refused `403 token subject
  does not match request patient`.
  Upgrade payer gateways before the provider gateways that send to them, as for
  v0.53.0.
- **Upgrade note (payer gateways).** A pended authorization recorded before the
  upgrade for a member your system of record does not hold was filed under the
  earlier identifier, so after the upgrade it is not found under the member's
  new one. An amendment for it still reaches your system and your answer is
  relayed, but it binds no pend and the gateway records nothing for it (it
  notes `pend.amendment-unbound`); an inquiry's decision for it is relayed but
  not recorded (it notes `pend.other-subject`). Before upgrading, let open
  pended authorizations for members your system of record does not hold reach
  a decision. Members it holds, and every exchange with no open pend, are
  unaffected.
- No configuration changes. `REQUIRE_KNOWN_MEMBERS=true` still refuses such a
  member instead.

### Optional diagnostic events (test tooling)

Diagnostic collection, and the `DIAGNOSTIC_*` settings that turn it on, are
test tooling: outside the promise. What follows describes it as it stands.

A `diagnostics.Queue` counts each sequenced event once as it leaves, and its
`Health` carries the counts (additive, omitted when zero): `acknowledged`,
`discarded` (declined as out of scope, or withheld by kind) and `droppedBy`, the drops by reason
(`queueFull`, `expired`, `oversized`, `unencodable`, `test`, `stopped`, and the
new `invalid`). `Health.CountsAgree` counts `invalid` among the drops.
`Queue.Drop` counts an expiry. The publisher drops an event as `invalid`, without
retrying it, when the ingest answers `400 invalid_event` or `409 conflict`; an
older publisher retried it until its ownership window ran out. An accounts
service older than these counts refuses a heartbeat that carries them.

`diagnostics.PublisherConfig.BatchURL` (additive; empty keeps one event per
request) names an ingest's batch endpoint. With it set, the first heartbeat is
sent at once, and once the ingest answers a heartbeat with the header
`X-SHN-Evidence-Batch` (`diagnostics.HeaderEvidenceBatch`) the publisher posts
up to 256 due events, bounded by 1 MiB of retained cost, in one request signed over its
whole body as `{"events":[…]}`, and settles each by the ingest's per-event
result (`accepted`, `binding_pending`, `scope_ignored`, `kind_not_admitted`,
`invalid`, `conflict`, `unavailable`). An event the ingest could not take now,
or a batch it refused whole, is due again after a backoff until each event's
own ownership window runs out; a test event the ingest did not take is not
retried. A `413` halves later batches, which grow back as batches settle in
full; a `413` for one event alone drops it as `oversized`. A sink that never sends that header (a
participant's own sink, or an ingest older than batches) gets one event per
request, as before. A batch answered 404 or 405, or with a success without one
result for each event in order, falls back to one event per request until a
heartbeat's answer carries the header again. The app sets `BatchURL` to its
sink URL's path plus `/batch` and `Drain` to 10 s, and its queue holds 4,096
events.

`diagnostics.PublisherConfig.Drain` (additive; zero keeps stopping at once)
bounds a final delivery after the publisher's context ends: it keeps sending
what is queued, an event or batch in flight at the stop included, until the
queue is empty or `Drain` has passed, then counts what is left as dropped
(`stopped`) and sends a last heartbeat with the counts. An event emitted after
the drain has ended is neither delivered nor counted; the app stops the
publisher last, after its listener has shut down and the gateway has closed. A
request still running when the listener's 5 s shutdown grace ends is cut, and
its handler has up to 2 s more to return, and emit, before the publisher stops.

A batch result of `kind_not_admitted` (the ingest never admits that kind from
this publisher) withholds the kind: its later events are counted as discarded,
and as `suppressed` in `Health` (additive, part of `discarded`), without being
sent, until ten minutes later, when its events go to the ingest again and its
answer decides afresh. No other result withholds a kind, and a sink that never
answers `kind_not_admitted` (one that does not take batches) gets every kind.
Events of the kind already queued when it is withheld are still sent.
`Health.CountsAgree` refuses `suppressed` above `discarded`, or without the
other counts. `Queue.TryEmit` returns `true` for an event of a withheld kind: it is
counted, not queued. `RunPublisher` uses `PublisherConfig.Clock` as the queue's
clock, read from emitting goroutines with the queue locked, so a `Clock` must be
safe for concurrent use and must not call into the queue. A collector that decodes
heartbeats strictly must know `invalid` and `suppressed` before a publisher sends
them.

The existing observer event names and completion barrier remain available.
Ingress observation now tees reads performed by the original handler, preserving
authentication ordering and read failures. The legacy ingress event fires when the original
handler finishes reading, before response commitment; unread rejected bodies are
reported on return as incomplete. Ingress events add `payloadIncomplete: true`
when request or response bytes were unread, truncated, failed, or unavailable due
to capture limits. An absent payload with this flag does not assert an empty body.
Complete events omit the flag and retain their existing JSON shape. Durable HTTP
events finalize on return. The existing observer
callback's panic behavior is retained, independently of the isolated diagnostic
sink. The SSE payload representation is unchanged; durable diagnostics carry raw
bytes directly. This source addition is not a published release or a version pin.

Relayed non-2xx responses preserve the participant's declared `Content-Type` with
its exact body bytes. The existing `application/fhir+json` fallback applies only
when that media type is absent; locally authored and empty-body refusal rules are
unchanged. This fixes a prior hardcoded media type on nonempty foreign errors.

### `PAYER_DAVINCI_BACKEND_HEADERS`

**Additive setting, v0.49.0: `PAYER_DAVINCI_BACKEND_HEADERS`** — fixed request headers
for a partner system that routes on one, sent on every request to the partner's bases and never
to its token endpoint (`docs/CONFIGURATION.md`, native-forward payer mode). Unset ⇒ every request
is byte-identical to an earlier release's; the option adds headers only, never changes a body, and a
deployment that does not set it is unaffected.

## Endpoints

The gateway serves HTTP on two listeners, and on a third, the Prometheus pull
listener, when monitoring turns it on. Which routes are mounted depends on
`ROLE` and on the settings that turn a route on: the Da Vinci ingress routes
and the ingress's own authorization endpoints are served by a provider's
gateway with `PROVIDER_DAVINCI_INGRESS` set (the authorization endpoints when
its ingress authorization is configured, and
`/.well-known/davinci-configuration` when `PROVIDER_DAVINCI_INGRESS_BASE_URL`
is set), the attachment-request Task routes and the same authorization
endpoints by a payer's gateway with `PAYER_DAVINCI_INGRESS` set,
`POST /substrate/inbound` by a
gateway that receives legs from the Hub, and the `/scenario/*` routes, which
are test tooling, only by a provider's gateway.

A pattern that gateways of two roles mount for different workstreams is listed
once for each role, as `PATTERN (ROLE)`, and classed by that role's workstream.
From shn-gateway v1.7.0, `POST /Task` is the one: a provider's federated query
and a payer's attachment-request Task.

From shn-gateway v1.8.0, a provider's gateway serves DTR's adaptive rounds at
`POST /Questionnaire/$next-question`, `POST /dtr/payer/{payer}/{rest...}` and
`POST /dtr/fetch-server/{payer}/{line}/{tag}/Questionnaire/$next-question`,
and its ingress CapabilityStatement at `GET /metadata` lists `$next-question`.

The main listener, on `HOST` and `PORT`:

| Item | Class | Owner |
|---|---|---|
| `GET /health` | core | core |
| `GET /internal/checks` | core | core |
| `POST /internal/checks` | core | core |
| `GET /internal/stats` | monitoring | core |
| `POST /substrate/inbound` | core | core |
| `GET /cds-services` | PA workstream | PA workstream |
| `POST /cds-services/{id}` | PA workstream | PA workstream |
| `POST /Questionnaire/$questionnaire-package` | PA workstream | PA workstream |
| `POST /Questionnaire/$next-question` | PA workstream | PA workstream |
| `POST /dtr/payer/{payer}/{rest...}` | PA workstream | PA workstream |
| `POST /dtr/fetch-server/{payer}/{line}/{tag}/Questionnaire/$next-question` | PA workstream | PA workstream |
| `POST /Claim/$submit` | PA workstream | PA workstream |
| `POST /Claim/$inquire` | PA workstream | PA workstream |
| `POST /CoverageEligibilityRequest/$submit` | PA workstream, evolving | PA workstream |
| `POST /$submit-attachment` | PA workstream, evolving | PA workstream |
| `POST /cdex/payer/{payer}` | PA workstream, evolving | PA workstream |
| `POST /cdex/payer/{payer}/$submit-attachment` | PA workstream, evolving | PA workstream |
| `GET /cdex/payers` | PA workstream, evolving | PA workstream |
| `POST /cdex/provider/{provider}/Task` | PA workstream, evolving | PA workstream |
| `POST /Task (payer)` | PA workstream, evolving | PA workstream |
| `GET /cdex/providers` | PA workstream, evolving | PA workstream |
| `GET /cds-client/jwks.json` | PA workstream | PA workstream |
| `GET /metadata` | core | core |
| `POST /oauth/token` | core | core |
| `GET /.well-known/smart-configuration` | core | core |
| `GET /.well-known/davinci-configuration` | PA workstream | PA workstream |
| `POST /Task (provider)` | FQS workstream | FQS workstream |
| `POST /scenario/uc01` | test tooling | test tooling |
| `POST /scenario/uc02` | test tooling | test tooling |
| `POST /scenario/uc02-payerb` | test tooling | test tooling |
| `POST /scenario/uc02-unknownpayer` | test tooling | test tooling |
| `POST /scenario/uc03` | test tooling | test tooling |
| `POST /scenario/uc04` | test tooling | test tooling |
| `POST /scenario/uc05` | test tooling | test tooling |
| `POST /scenario/uc06` | test tooling | test tooling |
| `POST /scenario/uc07` | test tooling | test tooling |
| `POST /scenario/uc07hcpcs` | test tooling | test tooling |
| `POST /scenario/uc08` | test tooling | test tooling |
| `POST /scenario/homeoxygen` | test tooling | test tooling |
| `POST /scenario/dispatch` | test tooling | test tooling |
| `POST /scenario/uc06/start` | test tooling | test tooling |
| `POST /scenario/uc06/complete` | test tooling | test tooling |
| `POST /scenario/uc06/cancel` | test tooling | test tooling |
| `POST /scenario/uc07/start` | test tooling | test tooling |
| `POST /scenario/uc07/complete` | test tooling | test tooling |
| `POST /scenario/uc07/cancel` | test tooling | test tooling |
| `GET /scenario/uc07/pending` | test tooling | test tooling |
| `POST /scenario/pa/inquire` | test tooling | test tooling |
| `POST /scenario/reset` | test tooling | test tooling |

The observer listener, served only when `OBSERVER_ADDR` is set, and only on a
loopback address:

| Item | Class | Owner |
|---|---|---|
| `GET /events` | core | core |
| `GET /health` | core | core |
| `POST /barrier` | core | core |
| `POST /demo/transform` | test tooling | test tooling |
| `GET /demo/capture/{correlationId}` | test tooling | test tooling |

The Prometheus pull listener, served only when `OTEL_METRICS_EXPORTER` names
`prometheus`, on `OTEL_EXPORTER_PROMETHEUS_HOST` and
`OTEL_EXPORTER_PROMETHEUS_PORT`; a bind other than loopback only over TLS,
with a bearer token, a client CA or both:

| Item | Class | Owner |
|---|---|---|
| `GET /metrics` | monitoring | core |

### Native PAS response contract

PAS operation responses must be complete Bundles containing exactly one
ClaimResponse and a closed set of referenced resources. **Every decision a payer
gives is relayed as the payer gave it**: the gateway does not poll its own payer
for a later decision, does not replace a payer's answer with one it assembled,
and stamps no contract line of its own on a message it did not produce. A payer
that pends answers `pended`, and the requester obtains the determination by
asking for it (`Claim/$inquire`, leg type `pas-claim-inquire`).

**Breaking in v0.46.0** (the assembly path is gone):

- The `leg.assembled` observer event is REMOVED. Nothing assembles a terminal PAS
  response, so nothing emits it. Consumers should drop the case; no event takes
  its place (`leg.response` already records the answer that was relayed).
- `engine.PASWaitDefault` is now `0` (was 30s). An originator route waits only when
  its caller asks it to. A caller that relied on the default to turn a pend into a
  determination now receives the pend and its continuation, and asks.
- `engine.PASWaitMax` is now 30s (was 120s). These still compile, so the change is
  silent at build time: a caller asking for more than 30s is capped rather than
  refused. Nothing is lost by it — the inquiry schedule always finished near 26s, so
  every wait above that was already a no-op that reported itself as a longer one.
  The bound and the schedule are now derived from each other and held together by a
  test, so they cannot drift apart again.

### The supplemental report on an amendment the gateway builds

**Evolving (v1.0):** amendments the gateway builds are module 1's (`module 1`).

**Behavior change in v0.57.0.**

- The `fhirsor` connector's `SupplementalReportContext` returns the report
  exactly as the FHIR server holds it, its subject included. Earlier releases
  replaced the report's `subject` with `{"reference":"Patient/<member id>"}`
  in the connector, whatever patient it named.
- When the gateway attaches that report to a PAS amendment it builds (UC-04),
  the only change it makes to the report's content is registered edit E-06
  (`evidence-subject-rekey`): `subject.reference` is re-pointed from the
  Patient your system holds the member under to `Patient/<member id>`, and
  only when the report names that Patient.
- The amendment is a bundle the gateway builds with the SDK's PAS update
  builder. From shn-gateway v1.8.0 it carries the report as it reaches the
  build, apart from E-06: under the id your system holds it by, with its
  `meta` (`meta.profile` included), member order and whitespace as written, at
  an entry whose `fullUrl` is built from that id, so the references in it
  resolve in the bundle under FHIR R4's Bundle rules. A report with no id is
  refused before the build, `500 supplemental report missing id`, as before;
  one whose id is not a FHIR id is refused by the build (`422`, the fidelity of
  the gateway's own build). Either way no amendment is sent. Releases before
  v1.8.0 gave the report a bundle-local id, dropped its `meta.profile` and
  re-encoded it (member order and whitespace) as they placed it in the bundle.
- The amendment is refused, and no amendment is sent, when the report has no
  `subject.reference` (`422 supplemental report names no subject.reference`);
  names any other subject (`422 supplemental report's subject is not the
  member's patient in the system of record`); carries a signature that
  covers the report (a `Signature` in the report itself, outside any resource
  it contains, or a signed `Provenance` it contains that targets it: `422
  signed content cannot be edited (E-06, <carrier>)`); or cannot be read as a
  resource (`502 supplemental report is not a resource`). Earlier releases
  rewrote the subject whatever the report named.
- **Breaking for a custom connector** that copied the earlier `fhirsor`
  behavior, rewriting the subject to `Patient/<member id>` while your system
  holds the patient under another id: that report no longer names the
  Patient your system holds, so the amendment is refused. Return the report
  as your system holds it. A connector whose system holds the patient under
  the member id, and returns the report naming `Patient/<member id>`, is
  unaffected.

### The facility's report on an amendment the gateway builds

**Evolving (v1.0):** amendments the gateway builds are module 1's (`module 1`).

**Behavior change in shn-gateway v1.8.0.**

- When the gateway attaches a facility's report, read on the federated-query leg,
  to a PAS amendment it builds (UC-05), the only change it makes to the report's
  content is registered edit E-14 (`facility-report-subject-rekey`): the report's
  `subject.reference` is replaced with the Claim's patient reference. The rest of
  the report, the `display`, `identifier` and `type` of its `subject` included, is
  the facility's as it reaches the build. Releases before v1.8.0 replaced the whole
  `subject` with `{"reference":<the Claim's patient>}`, dropping its other members,
  and named no edit.
- The amendment is a bundle the gateway builds with the SDK's PAS update builder,
  which carries the report and the facility's Provenance as they reach the build,
  apart from E-14: under the facility's own ids, with their `meta` (`meta.profile`
  included), member order and whitespace as written, each at an entry whose
  `fullUrl` is built from its own id, and the Provenance's `target` as the facility
  wrote it. A report or Provenance the build cannot carry so is refused (`422`,
  the fidelity of the gateway's own build), and no amendment is sent: one with
  no id (the gateway never mints one), one that is not exactly one JSON object
  (a member named twice or spelled in another case among them), one that is not
  UTF-8 or holds a lone surrogate escape, or one past the build's own limits
  (16 MiB, nesting 64 deep, 1,000,000 JSON tokens). The provider's gateway writes no reference of the
  bundle in absolute form for any payer: the references resolve under FHIR R4's
  Bundle rules. Releases before v1.8.0 gave the report and the Provenance bundle-local
  ids (the Provenance's `target` rewritten to match), dropped their
  `meta.profile`, re-encoded them (member order and whitespace) and, on the
  origination profiles that relay the reference payer's bytes, wrote the bundle's
  references, the report's `subject.reference` among them, in absolute form, so a
  signature over the report or that Provenance no longer verified in the
  amendment, whether or not E-14 was made. A facility's Provenance whose `target` is
  version specific (`DiagnosticReport/<id>/_history/<version>`) is now carried so; a
  payer's gateway before v1.8.0 reads no version-specific target, so at `strict` it
  refuses that amendment (`pas.update-provenance`), where earlier requesters had
  rewritten the target to the report's bundle-local id.
- The edit is made only once the report, and the Provenance that attributes it,
  are shown to be about the Claim's patient, as before: the report's subject names
  the member directly, or through a Patient the facility carried with the member
  identifier. A report about anyone else is refused, as before, with no edit made
  (`422`, the fidelity of the gateway's own build).
- A report whose `subject.reference` is exactly the Claim's patient reference is
  carried as the facility wrote it, and so is one whose `subject` names the member by
  identifier alone, with no `reference`. Releases before v1.8.0 turned such a
  subject into a reference.
- A report a signature covers (a `Signature` in the report, a signed `Provenance`
  it contains that targets it, or the facility's signed `Provenance` that
  attributes it) is refused when the edit would change it, `422 signed content
  cannot be edited (E-14, <carrier>)`, and no amendment is sent. A signed report
  already naming the Claim's patient needs no edit and is carried as the facility
  wrote it, as above.
- The access line's `relayEdits` and `requestEdits` name `E-14` on an amendment
  that carries a report E-14 changed. The amendment stays `ownership: authored`
  with its `builder`, since E-14 is made to the report before the build.
- The facility's Provenance is carried with its `target` exactly as the facility
  wrote it, whatever it names: the provider's gateway never rewrites or judges it.
  Whether it names the report is the payer's content check (`pas.update-provenance`):
  exactly `DiagnosticReport/<id>` or exactly the report's entry `fullUrl`, either
  one version specific (`…/_history/<version>`) only at the report's own
  `meta.versionId`. The report rides under its own id at the amendment's base, so a
  `target` written as the facility's own `fullUrl` (its server's, or the federated
  query's `urn:shn:fedquery:<n>`) names no entry of the amendment and the payer's
  check reads it so. Releases before v1.8.0 rewrote the `target` to the report's
  bundle-local id, whatever it named.
- **Compatibility:** a payer whose own system cannot resolve relative references
  sees its own system's refusal, carried as sent, for a request a provider's
  gateway from v1.8.0 builds.

### The evidence a provider-data request carries

**Behavior change in shn-gateway v1.7.0.**

- On the provider-data origination profile (`ORIGINATION_PROFILE=provider-data`), a PAS
  submission or update the gateway builds carries the clinical records it references
  (each Condition, Observation, ClinicalImpression, Goal or DiagnosticReport) as your
  system of record holds them. When your system holds the member's Patient under its
  own id, the only change to such a record is registered edit E-06: its own
  `subject.reference`, when it is exactly that Patient, is re-pointed to
  `Patient/<member id>`, as the request names the patient. A resource contained in the
  record is not edited.
- A record naming anything else (another patient, `Patient/<member id>` itself, a
  versioned or absolute reference) is refused, `502 PAS evidence linkage failed`, and one
  a signature covers is refused, `422 signed content cannot be edited (E-06, <carrier>)`,
  each before the claim is sent. Before v1.7.0, a request with a record about the
  Patient your system holds the member under was refused with `502 PAS evidence linkage
  failed`, after the CRD and questionnaire legs had gone out, and a record naming
  `Patient/<member id>` while your system holds the member under another id was carried.
  In your system's own numbering that is another record, so it is now refused.
- A system that holds the member's Patient under the member id is unaffected.
- From v1.8.0 every PAS submission and amendment the gateway builds is checked once,
  right after the build, for two edits of its own build that cannot be made, each
  refused `422`, the fidelity of the gateway's own build, with nothing sent. First, a
  record the build addresses by its own `resourceType` and `id` (the Coverage, the
  payer's Organization, the Claim's provider) that states either only in another
  spelling (`"ID"`, say), or any record whose `resourceType` is spelled so: such a
  record has no FHIR id, and the build would address it by an id the participant never
  gave (`it states its resourceType or id only in another spelling`). Second, a record
  the build gives an id of its own (the order, the QuestionnaireResponse, and the
  Provenance of an amendment with no report) that spells its id another way, alone or
  beside its own `id`: the build's id would sit beside that spelling, in a body the
  network's message integrity does not seal, since it can be read two ways (`the build's
  own id for it cannot be set beside the id it spells another way`). Once the build
  carries these records as written, a lone member spelled so is carried. Each message
  follows `422 a record read from the system of record cannot be carried as read: `.
  Earlier releases, depending on the request, built and sent such a request or answered
  it as the gateway's own `500`. A person the Coverage names spelled so is, as before,
  refused `422` by the build's own party read.
- From v1.8.0 the records the gateway adds to a request it built (these clinical
  records, the records the order's requester leads to and a dispatched supplier, each
  where the request does not already hold it, as the Claim's provider, which the build
  re-encodes, say) are spliced into the request's bytes as read. One that reaches the
  splice and cannot be carried as read (one that is not exactly one JSON object, is not
  UTF-8 or holds a lone surrogate escape, or names a member twice whatever its case; an
  order's requester or a dispatched supplier with no `resourceType` or `id` spelled
  exactly) is refused `422 a record read from the system of record cannot be carried as
  read: <why>`, the fidelity of the gateway's own edit, and the claim is not sent. Most
  such records are answered earlier, as in earlier releases, by the read that takes them
  from your system or by E-06.
- From v1.7.0 the access line's `relayEdits` names `E-06` on a request the gateway built
  that carries a record E-06 re-pointed, these records or the supplemental report an
  update carries. The request stays `ownership: authored` with its `builder`, since E-06
  is made to the record before the build. Earlier releases re-pointed the supplemental
  report's subject but did not name the edit there; the provider-data evidence is
  re-pointed only from v1.7.0.

### Refusals of a request frame are framed

**Behavior change in v0.57.0.**

- A refusal the gateway writes about a request frame after the leg is
  authenticated and decrypted, before any leg handler runs, is framed as its
  answer with `200` to the Hub, like a leg handler's refusal. The refusals are:
  a contract line it cannot build (`422`); one it has no validator lane for
  (`422`); a claim on a version-neutral leg (`422`); a frame that does not
  decode (`400 request frame decode failed`); and an operation header on a leg
  that defines none (`400 operation header is not defined for this transaction
  type`).
- A frame-capable requester's gateway now relays the status and reason to its
  participant. Earlier releases wrote these bare, and the Hub reported its
  failed forward. A requester that negotiated no frame is unaffected.
- The no-validator-lane reason no longer ends in the requirement identifiers
  `(FR-36/FR-G29)`; it now reaches your participant, so it names only the
  line and the refusal.
- No configuration or Go API changes.

### The payer's media type on a relayed answer

**Evolving (v1.0):** what this section says of the eligibility answer
(`PA workstream, evolving`); the CRD, DTR and PAS answers are full.

**Behavior change in v0.57.0.**

- A payer's gateway frames a success answer it relays from its payer's system
  with the media type that system stated, instead of always
  `application/fhir+json`, on every leg: a CDS Hooks answer, a questionnaire
  package, a PAS answer to a claim, an amendment or an inquiry, and the
  eligibility answer of a payer that declares its own endpoint. An answer the
  gateway built, or one that states no type, is still framed
  `application/fhir+json`.
- The provider's CRD ingress writes the payer's stated type to the EHR (the
  reference payer answers `text/json;charset=UTF-8`) instead of always
  `application/json`. A CDS Hooks answer is never a FHIR resource, so one
  framed `application/fhir+json` (as every payer gateway before v0.57.0 frames
  it), stating no type, or stating one that does not parse as a media type is
  still written as `application/json`.
- The provider's DTR and PAS ingress still write `application/fhir+json` to
  the EHR, the type of the FHIR resources they answer with, whatever type the
  frame carries.
- No configuration or Go API changes.

**Behavior change in v1.10.0.**

- A requesting gateway writes the media type a success frame states to its own
  participant's system, whatever it is, as a direct client would see it:
  - on the provider's DTR and PAS ingress (a questionnaire package, a
    `$next-question` answer, a PAS answer to a claim, an amendment or an
    inquiry), where earlier releases wrote `application/fhir+json` whatever the
    frame carried. This changes a full item: v1.10.0's fourth recorded
    exception (Versioning policy, above);
  - on the CDex `$submit-attachment` and data request routes, coverage
    eligibility and the federated query, where earlier releases wrote
    `application/fhir+json` unless the frame stated `application/json` or
    `application/fhir+json`.

  A frame that states none, or a type no header can carry, is written
  `application/fhir+json`.
- The provider's CRD ingress writes a CDS Hooks answer stated
  `application/fhir+json` as stated when the payer's gateway advertises the
  `v1opsub` request frame, as a payer's gateway does from v1.8.0, and
  `application/json`, as before, from any other payer, whose gateway may
  predate v0.57.0. A stated type no header can carry is written
  `application/json`. This is part of the same exception.
- No configuration or Go API changes.

### An empty application error body

**Behavior change in v0.57.0.**

- When a participant's system answers a leg non-2xx with an empty body, the
  payer's gateway relays it as it came: the requester's gateway receives the
  status, an empty body, and the media type the system stated (or none), and
  writes the same to its participant. Earlier releases wrote
  `{"error":"<leg>: recipient answered <status> with no error detail"}` in its
  place, as `application/json`.
- A non-empty answer is unchanged: its bytes and media type are relayed, and
  one that states no media type is still read as `application/fhir+json`.
- The gateway's own refusals (no answer from its system to relay) keep their
  `{"error": …}` body, which names the leg and status when there is no reason
  to give. (From v1.10.0 that body takes the leg's answer envelope: see
  "Behavior change in v1.10.0" below.)
- A requester that negotiated no frame still sees the Hub's failed forward. The
  bare answer the Hub discards is the gateway's own refusal,
  `{"error":"<leg>: recipient answered <status>; its answer is carried only in
  a message frame"}`, never the participant's bytes: a bare answer reaches the
  Hub unsealed. Earlier releases wrote a gateway `{"error": …}` body there
  too, with other wording.
- No configuration changes. The interim relay builder
  `defect-empty-error-substitution` is retired, so no frame may carry the
  gateway's own body in place of a participant's application error. Its
  exported constant `relay.BuilderInterimEmptyErrorSubstitution` remains,
  deprecated, so code naming it still compiles; `relay.Authored` refuses it.

**Behavior change in v1.10.0.**

- The gateway's own refusal of a leg, with no answer from its system to carry,
  is written in the leg's answer envelope, a field of each leg in the network's
  leg taxonomy, which the participant protocol's §4.3 names: on coverage
  eligibility, DTR,
  PAS, CDex and the federated query, an `OperationOutcome`
  (`application/fhir+json`) whose first issue is the refusal (severity `error`,
  a code from its status, the message as its `diagnostics`), then one `invalid`
  issue for each validator issue it echoes; on the CDS Hooks legs and
  patient-authored DTR, `{"error": …}` (`application/json`) as before. A
  requester's gateway relays either as it arrived, so a client of a FHIR
  operation route reads a counterpart gateway's refusal as it reads its own
  gateway's. The bare answer to a requester that negotiated no frame takes the
  same envelope.
- Statuses, messages, refusal rules and the exchange record are unchanged, and
  a participant's own answer is carried as it was sent on every leg.
- While a counterpart runs an earlier release, a client of a FHIR operation
  route can receive either body: read an `OperationOutcome` first, then `error`.
- No configuration changes.

### Amendments a provider's gateway builds, after a payer's 409

**Evolving (v1.0):** amendments the gateway builds, and their re-send, are
module 1's (`module 1`).

**Behavior change in v0.56.0.**

- When a provider's gateway builds a PAS amendment (`pas-claim-update`) for its
  participant (the scenario flows, and resuming a pended request with a
  clinician's or the patient's answers) and the payer answers `409` (a version
  conflict: its store refused the amendment while it resolved the same claim,
  and kept nothing), the gateway builds and sends the amendment once more under
  a new correlation id, and relays the payer's answer to that one. A second
  `409` is relayed as it came.
- Each attempt is its own leg, with its own `leg.originated`/`leg.response`
  pair and its own Hub records. The new `leg.resent` observer event
  (`engine.LegResentEvent`) links them: its correlation id is the re-send's, and
  its `Detail` names the refused attempt's (`refusedCorrelationId`).
- An amendment the participant sends through the Da Vinci ingress is relayed as
  before: its `409` reaches the participant, who decides whether to resend. A
  payer's gateway relays its payer's `409` and never resends (v0.54.0).

### CDS Hooks answers on the legs a provider originates

**Evolving (v1.0):** the legs a provider's gateway originates are module 1's,
and the checks on their answers module 3's (`module 1`, `module 3`).

**Behavior change in v0.56.0.**

- A provider's gateway checks the payer's CDS Hooks answer on a CRD leg it
  originates itself (`order-sign`, `order-select` and `order-dispatch`) against
  the CDS Hooks response rules, at its own conformance level, before it reads
  the payer's coverage information from it. It already did so for an answer it
  relays to its participant's own system through the Da Vinci ingress, and a
  payer's gateway does so for its own system's answer.
  - `none`: no check (a repeated member name, which is message integrity, is
    still refused at every level).
  - `observe`: each broken rule is recorded as a finding, and the exchange
    continues.
  - `structural`: a broken structure is refused (`502 payer CRD response is not
    a valid CDS Hooks response: <rule>`); a card's summary length, CRD topic
    and selection behavior, and an action's resource, are recorded.
  - `strict`: any broken required rule is refused.
- There is no exception for the network's reference payers: their gateways
  already pass their own answers at `strict`. (The reference-payer exception
  below applies only to their DTR and PAS answers.)
- **Compatibility.** A provider gateway at `structural` or `strict` paired with
  a payer whose CDS Hooks answers break a required rule, and whose own gateway
  does not refuse them (it runs `none` or `observe`), now refuses those answers
  on the legs it originates, where earlier releases read and acted on them.
  Some payers' CRD answers today omit a member CDS Hooks requires (a system
  action's `description`, or `cards` itself), so a provider at `structural` or
  `strict` paired with such a payer sees `502` on these legs. This includes a
  SHN Kit provider set to `structural` or `strict`, once the Kit pins v0.56.0 or
  later; the Kit's default is the gateway's own, `observe` before v1.0.0. At
  `observe`, the hosted and Kit default before v1.0.0, only new findings appear.
- No configuration or Go API changes. An answer relayed through the Da Vinci
  ingress is checked once, as before.

### A payer's DTR and PAS answers on the legs a provider originates

**Evolving (v1.0):** the legs a provider's gateway originates are module 1's,
and the checks on their answers module 3's (`module 1`, `module 3`).

**Behavior change in v0.55.0.**

- A provider's gateway builds and sends some legs itself, on its
  `ORIGINATION_PROFILE` lane (`demo`, which is what unset meant for
  `ROLE=provider` before v1.0.0, or `provider-data`). It now checks the payer's answer on
  these legs at its own conformance level: the DTR `$questionnaire-package`
  answer, the DTR `$next-question` answer, and the PAS ClaimResponse answer to a
  submit or update, including on a resumed exchange. Earlier releases checked
  no payer's answers on these lanes. From v0.56.0 the payer's CDS Hooks answer
  on the CRD legs it originates is checked too (above).
- **Not checked:** `$inquire` answers, and the answers a gateway relays to its
  participant's own system through the Da Vinci ingress (the native PAS and
  questionnaire relays), as before.
- **Reference payers.** An answer is not checked only when both of these hold:
  the gateway is on the `demo` or `provider-data` lane, and the payer identity
  the member's Coverage names, which the leg was routed by, is one of SHN's
  reference payers: `urn:oid:2.16.840.1.113883.6.300` `00001`, `00300` or
  `00301`, or one of SHN's two bridging-demo payers under `urn:shn:demo-payer`
  (`SHN-BRIDGE-DEMO`, `SHN-BRIDGE-REFUSE`), which front `00001`. Their packages
  do not yet conform: the 2.0 reference payer's DTR package fails DTR 2.0.1. The
  set changes only in a gateway release, never through configuration.
- **Lines tried, in order.** Each answer is checked against the IG lines the
  gateway can validate:
  1. the line the leg was sent at: the payer's declared line, or the gateway's
     own line when the payer declared none;
  2. the lines the answer claims, first through a versioned `meta.profile`, then
     through a structural marker whose claim is narrower than every line;
  3. the rest of 2.2, 2.1 and 2.0.
- **Claims.** What an answer claims is read as claims, each a set of lines met
  when any one of them is judged. A versioned `meta.profile` claims its line. A
  structural marker claims the lines whose IG packages, as the validator lanes
  load them, define it where it sits: `qr-context` on a QuestionnaireResponse,
  `extension-certificationType` on a Claim or its item, and
  `extension-serviceItemRequestType` on a Claim's item claim every line, and
  `qr-coverage` on a QuestionnaireResponse claims 2.2. No other extension claims
  a line. From v1.0.0; before, the first three claimed 2.1 alone.

  Each line is tried at most once, and only where the gateway has a validator
  for it. A validator that serves several lines is called only once, so a
  gateway with one validator for every line judges the answer once, at the line
  the leg was sent at. The check stops at the first line where the answer is
  valid, and a further line is tried only when the line before it was not
  valid, except that after a valid line a further line is tried while it could
  still meet a claim a line still qualifying left unmet (the verdict below). The line the leg was sent at uses the validator client's own timeout;
  every other line gets at most 2 seconds, and one that does not answer in time
  counts as unavailable for that line. So when the gateway has no validator for
  the line the leg was sent at, the first line it actually checks gets the
  2-second bound.
- **The verdict.** An answer that is valid on any line is valid, except, from
  v1.0.0, while the line the leg was sent at, or a claim none of whose lines was
  judged, has a default validator lane that has not qualified yet: the check is
  then unavailable whatever another line says, because the line that decides it
  cannot be checked yet. If no line is valid, the check is unavailable when the
  line the leg was sent at could not be checked, or a claim the answer makes was
  met by none of its lines (no validator answered for any of them, or the
  gateway has none), and also when no line's validator answered.
  Otherwise the verdict is the best one any line gave; a deeper-rule defect
  beats a structural one.
- **By level:**
  - `none`: no validator call and no finding.
  - `observe`: every outcome that is not valid is recorded, and the answer is
    relayed.
  - `structural`: a structural or unclassified defect is refused; a deeper-rule
    defect and an unavailable check are recorded; the answer is otherwise
    relayed.
  - `strict`: every defect is refused, and an unavailable check is refused with
    `500`.
- **A refusal.** The gateway's own client receives `422 {"error":"ingress
  validation failed: <issues>"}`, with the issues from the line that decided. An
  unavailable check at `strict` answers `500` (`validator unavailable`, or a
  message naming the line that has no validator lane). The payer has already
  received the request and answered it.
- **Findings.** An answer valid at the line the leg was sent at records nothing.
  Any other checked answer records one conformance finding, except an
  unavailable check at `strict`, which is refused without one. The finding
  carries `declaredLine` (the line the leg was sent at), `lines` (each line
  considered, in order, with its verdict: `valid`, `structural`, `deeper` or
  `unavailable`; the line the leg was sent at, or a line of a claim none of
  whose lines has a validator, is listed as `unavailable` when it has none, and no validator diagnostic
  appears here) and `line` (the line the decision came from). An answer valid
  only on a line after the one the leg was sent at gets one finding with
  `verdict` `valid` and `decision` `record`; this is not a defect. When the
  gateway has no validator for any of the lines, the answer is judged once, as
  before v0.55.0, and its finding carries no `declaredLine` or `lines`.
- **A known limit.** An answer valid on any supported line is relayed at every
  level, apart from the qualifying-lane exception above. For example, an answer sent at 2.2 that is structurally invalid at 2.2
  but valid at 2.0 is relayed at `strict`. A payer's declared line may be the
  gateway's default rather than the payer's own claim, and the network cannot
  yet tell the two apart.
- **Compatibility.** At the default level before v1.0.0 (`observe`) the check records findings
  and refuses nothing; the validator calls add time. At `structural` a
  structurally invalid answer is refused (`422`), and at `strict` any defect is
  refused (`422`), as is an answer the validator cannot check (`500`). The check
  is the provider's gateway's own and does not depend on the payer's release. A
  provider's gateway before v0.55.0 on the `demo` or `provider-data` lane checks
  no payer's answers.
- No configuration changes. See also
  [docs/INTEGRATION.md](docs/INTEGRATION.md#conformance-enforcement).

### Patients an exchange involves

**New in v0.55.0 (additive).**

- When the network's discovery descriptor lists `involved` in `hubAccepts`,
  each prior-authorization leg names, in its envelope's `involved` list, the
  other patients it involves, each with its own token, so the network records
  the exchange under each of them as well as the leg's own patient:
  - on a request, every other member the request carries, identified through
    your system of record as the leg's subject is (`request-named`). From
    shn-gateway v1.6.0, one your system does not hold as a member is
    never recorded under an identifier derived from its id alone, and on every
    leg but coverage eligibility it is left out and counted, a Patient carried
    with demographics but not held included: every other patient is named only
    under the identity your system holds for it. From shn-gateway v1.6.0, on
    PAS submit and update the leg's own patient is decided by the request's
    structure (the operative
    Claim's `patient` and the one Patient entry it resolves to); on CRD, DTR
    and the PAS inquiry a held patient is also left out and counted when a
    Patient the request carries, however carried, ties its name to one of the
    leg's own patient's names (on CRD `context.patientId`), to a name that
    identifies no one or whose read fails, or to a name tied in turn, and each
    distinct Patient under the leg's own patient's names beyond the first is
    counted. A Coverage's
    party, a contained Patient or (from shn-gateway v1.4.0) a
    `Patient` entry of its own that only a Coverage's `subscriber` or
    `policyHolder` names, is no member, and neither is the slot naming it (from
    shn-gateway v1.6.0, also a slot naming the party by a bare
    reference the request does not carry); but
    from shn-gateway v1.3.0 a patient the party names is (for a party carried as a
    `Patient` entry of its own, from v1.4.0), wherever the
    request is carried: gateways v0.61.0 to v1.2.0 (to v1.3.0, for a party
    carried as an entry of its own) left such a patient out, so
    the network had no record of the exchange under them (at `strict` the PAS
    legs refuse such a request first, under `patient.mixed`);
  - on a payer's answer, the payer's own binding of the member, when it differs
    from the patient the leg's token names (`payer-held` or `payer-derived`).
- It runs at every conformance level, `none` included: it is the network's
  audit record, not a check of your payload. It never refuses a request, never
  records a conformance finding, and never changes the bytes exchanged. On the
  requester side it reads your system of record once for each distinct member
  a request carries, up to 32.
- The whole pass for one leg runs within 2 seconds (token requests four at a
  time), so it cannot hold a leg past that.
- A patient it cannot name is left out and the leg is sent as usual: past 16
  patients, a system-of-record read that fails, a member not held under
  `REQUIRE_KNOWN_MEMBERS=true`, from shn-gateway v1.6.0 one it cannot
  identify (above), a token that is refused, cannot be obtained,
  does not verify for this leg and patient (from v1.1.0), or does not carry the
  involvement asked for, or one not named before the 2 seconds run out. Each
  counts on the call's exchange record and access line (`involvedOmitted`, from
  v1.1.0), raises the observer event `involved.omitted` (Detail: the reason)
  and, with `METRICS_SERVICE` set, counts in the EMF metric
  `InvolvedOmitted{reason}`.
- The descriptor is read once, at start, and the boot line says which way it
  went. A gateway started before the network lists `involved` sends nothing
  until it restarts.
- No configuration changes. Without `involved` in `hubAccepts` a leg carries
  no list, reads nothing more, and requests no further token.

### Bridging refusals answer `422`

**Evolving (v1.0):** bridging is module 2's, reachable here only on a request
module 1 builds (`module 1`, `module 2`); that a bridge which would change what
a request asserts is refused, and nothing sent, is a network rule and full.

**Behavior change in v0.55.0.**

- When a request cannot be carried to the recipient's IG line without changing
  what it asserts, the gateway refuses it and sends nothing. That refusal now
  answers `422`, where it answered `502`. The body is unchanged:
  `{"error":"shn: semantic-change refusal: …"}`.
- On the wire, a leg is bridged only when the recipient's line is not one the
  gateway carries natively. Every published line is native, so today this is
  reachable only on a PAS submit or update the gateway builds itself while its
  egress is narrowed with the demo-only `SHN_DEMO_EGRESS_NATIVE_LINES` (see
  [docs/CONFIGURATION.md](docs/CONFIGURATION.md)). A DTR `$next-question` or
  questionnaire-package fetch is carried to the payer's line unchanged, so it
  never takes this refusal.
- Any other failure while bridging is the gateway's own fault and stays `502`: a
  step that cannot parse the payload, a missing chain, or a Provenance that does
  not round-trip.
- The bridging demo lane keeps its structured `200` refusal.

### A Coverage named twice in a DTR QuestionnaireResponse

**Evolving (v1.0):** bridging between IG lines is module 2's (`module 2`).

**Behavior change in v0.55.0.**

A DTR QuestionnaireResponse can name its Coverage both in `qr-coverage` and in a
Coverage `qr-context` entry. When the gateway's transform chain bridges one
between 2.1 and 2.2, in either direction:

- **Fold.** A single Coverage `qr-context` entry is removed when it names exactly
  the same Coverage reference as the `qr-coverage` entry and equals it apart from
  its `url`. The `qr-coverage` entry keeps its position: going up it stays in
  place, and going down it becomes the `qr-context` entry in place. Earlier
  releases left two entries naming the same Coverage.
- **Refused**, as a semantic-change refusal:
  - two different Coverages, in either extension;
  - references that are not exactly equal, such as an absolute and a relative
    reference, or an identifier-only reference;
  - going up, a `qr-context` entry that names a Coverage by anything but a
    relative `Coverage/<id>` reference; going down, one that does so and does not
    exactly repeat the `qr-coverage` reference;
  - more than one Coverage `qr-context` entry;
  - a repeat that differs from the `qr-coverage` entry in anything but its `url`;
  - a result that would name one Coverage twice.
- The 2.2 to 2.1 direction could not refuse before. It now refuses these shapes.
- With no repeat present, the single Coverage entry is relocated in place, as
  before.
- **Where it runs.** No transmitted leg bridges a DTR QuestionnaireResponse
  today: a DTR questionnaire fetch is carried to the payer's line unchanged, and
  a PAS bundle's embedded QuestionnaireResponse is left as sent. The fold and its
  refusals apply to the transform chain itself (`engine.RunTransformChain`, and
  the observer listener's `POST /demo/transform`, which answers a refusal with
  `422` and its own body).

### The Da Vinci ingress's no-coverage refusals

**Behavior change in v1.1.0.**

- A CDS Hooks request with no coverage the gateway can route by is refused
  `412` with one of these reasons:
  - `no coverage in request or system of record`: a `null` coverage prefetch,
    or none the participant's system of record holds for a member it holds;
  - `no coverage to route by: send prefetch.coverage or fhirServer (…)`: a
    member the system of record names no patient for (one it does not hold, or
    holds but cannot name), when the request names no `fhirServer`;
  - `no coverage to route by: send prefetch.coverage (…, and it does not read
    fhirServer)`: the same member, with `CDS_FHIR_SERVER_READ=off`.

  The first reason, and each other one up to its parentheses, is inside the
  promise, as the participant protocol documents it. The explanation in parentheses is outside
  the promise; a release that rewords it says so in
  [`CHANGELOG.md`](CHANGELOG.md). The refusals of the read through `fhirServer`
  also begin `no coverage to route by:`, with their own statuses and reasons
  (see [docs/CONFIGURATION.md](docs/CONFIGURATION.md)); this section does not
  cover them.
- From v1.1.0 the explanation reads `(this gateway's records name no patient
  for this member)`, and `(this gateway's records name no patient for this
  member, and it does not read fhirServer)` with the read off. v1.0.0 said
  `(this gateway's system of record names no patient for this member)` and
  `(this gateway's system of record names no patient for this member, and it
  does not read fhirServer)`. Statuses and conditions are unchanged.

### When the requester stops waiting

**Evolving (v1.0):** what this section says of the coverage eligibility
forward (`PA workstream, evolving`); the CRD, DTR and PAS calls are full.

**New in v0.55.0.**

- A payer's gateway still waiting on its payer's system when the request it
  serves ends now logs its own line. The request ends when the requester stopped
  waiting or when the payer's gateway is shutting down:

  `gateway: upstream payer <leg label> call abandoned after <s>s: the request it serves ended (<cause>) (host <host>, leg <leg>, correlation <id>, request written: yes|no)`

- It covers the CRD, DTR, PAS submit, update and inquiry calls, and the coverage
  eligibility forward. It names the upstream host only, never the path, query,
  headers or body. Earlier releases wrote no payer-side line for it.
- It is written alongside the existing `upstream payer … unreachable` or
  `read failed` error, when the request had already ended; those errors are
  unchanged (see [docs/CONFIGURATION.md](docs/CONFIGURATION.md)).

## Observer event feed

With `OBSERVER_ADDR` set, the gateway serves its observer event feed as
server-sent events at `GET /events` on the loopback observer listener; without
it, there is no feed. Each event is a JSON object whose `kind` names what
happened. Fields, event kinds and values may be added in minor releases, so
decode events tolerantly and ignore a kind you do not know. The feed is an
evolving surface (Versioning policy, above): a minor release may also remove
or rename a kind, a field or a value, change what one means, or narrow a closed
set, with a [`CHANGELOG.md`](CHANGELOG.md) note. It graduates once a consumer
outside SHN, or the SHN Kit's event inspector at its own v1.0, depends on it:
then each of its items takes its class's tier, so a module's kind (such as
`leg.resent` or `leg.certified`) stays evolving. The first table
lists the event kinds. The second lists the values an `ingress.received` or
`ingress.responded` event carries in `legType`, naming the ingress route the
call arrived on. The third lists the values a `pend.amendment-unbound` event
carries in `detail`: why the amendment bound no pend this gateway recorded
(`docs/CONFIGURATION.md`). An event's `detail` is free text for an operator,
except for `pend.amendment-unbound`, whose `detail` is one value of the third
table.

| Item | Class | Owner |
|---|---|---|
| `leg.originated` | core | core |
| `leg.response` | core | core |
| `leg.failed` | core | core |
| `leg.resent` | module 1 | module 1 |
| `leg.refused` | core | core |
| `contract.line-carried` | core | core |
| `leg.downgrade` | core | core |
| `leg.transformed` | module 2 | module 2 |
| `leg.certified` | module 3 | module 3 |
| `relay.ownership.refused` | core | core |
| `ingress.received` | core | core |
| `ingress.responded` | core | core |
| `validate.result` | module 3 | module 3 |
| `sor.read` | core | core |
| `conformance.observed` | module 3 | module 3 |
| `involved.omitted` | core | core |
| `subject.binding-differs` | core | core |
| `cdex.requester-differs` | PA workstream, evolving | PA workstream |
| `cdex.owner-differs` | PA workstream, evolving | PA workstream |
| `dtr.adaptive-address-carried` | PA workstream | PA workstream |
| `crd.embedded.validated` | module 3 | module 3 |
| `prefetch.obtained` | PA workstream | PA workstream |
| `peer.nonconformant` | module 3 | module 3 |
| `pa.local-write-skipped` | PA workstream | PA workstream |
| `pa.local-write-failed` | PA workstream | PA workstream |
| `pa.inquiry-failed` | module 1 | module 1 |
| `pa.continuation-write-failed` | module 1 | module 1 |
| `dtr.referenced` | module 1 | module 1 |
| `pend.other-subject` | PA workstream | PA workstream |
| `pend.inquiry-unmatched` | PA workstream | PA workstream |
| `pend.lookup-unavailable` | PA workstream | PA workstream |
| `pend.decision-not-recorded` | PA workstream | PA workstream |
| `pend.amendment-unbound` | PA workstream | PA workstream |
| `pend.no-lookup-keys` | PA workstream | PA workstream |
| `decision.conflict` | PA workstream | PA workstream |
| `decision.superseded` | PA workstream | PA workstream |
| `decision.stale-answer` | PA workstream | PA workstream |
| `cdex.payer-id-differs` | PA workstream, evolving | PA workstream |

| Item | Class | Owner |
|---|---|---|
| `crd-ingress` | PA workstream | PA workstream |
| `dtr-ingress` | PA workstream | PA workstream |
| `pas-ingress` | PA workstream | PA workstream |
| `pas-inquire-ingress` | PA workstream | PA workstream |
| `eligibility-ingress` | PA workstream, evolving | PA workstream |
| `cdex-ingress` | PA workstream, evolving | PA workstream |
| `cdex-request-ingress` | PA workstream, evolving | PA workstream |
| `federated-query-ingress` | FQS workstream | FQS workstream |

| Item | Class | Owner |
|---|---|---|
| `not-pended` | PA workstream | PA workstream |
| `update-in-progress` | PA workstream | PA workstream |
| `already-decided` | PA workstream | PA workstream |
| `ledger-unavailable` | PA workstream | PA workstream |
| `no-prior-claim` | PA workstream | PA workstream |
| `other-requester` | PA workstream | PA workstream |

### The observer stream

**Observer stream** (`OBSERVER_ADDR`, `engine.Config.Observer`, `ObserverEvent` JSON,
`observer.Hub`): new in v0.19.0 and **evolving** — field additions and event-kind
additions may happen in minor releases. The SHN Kit's `shnkitd` daemon (`kit/relay`) is now
this stream's first real consumer: a local desktop inspection tool that SSE-subscribes to a
provider-role gateway child's `/events` and re-emits frames onto its own run-timeline bus,
stamped with the active run's identity. That consumer stays payer-role-aware (a payer-role
gateway's stream is validation-only — `kit/relay`'s package doc). The feed's event JSON is
binary contract on the evolving terms above; the Go hooks behind it are SHN-internal.

**v0.26.0** adds the `sor.read` event kind (the gateway's `SystemOfRecord` reads, one event
per call) — an event-kind addition, covered by the evolving-contract clause above.

**v0.34.0** adds `POST /demo/transform` on the same observer listener (`engine.RunTransformChain`
exported for it) — a loopback-only JSON shim over the real compat-chain machinery, not itself
part of the SSE stream (a run through it never appears on `/events`). `shnkitd`'s
`POST /api/bridging/exhibit` is now this endpoint's first real consumer, proxying it
over embedded reference content so the Kit's engine exhibit provably runs "the same modules
your live legs route through" — same binary, same manifest. `/demo/transform` is test tooling,
outside the promise; consumers pin exact gateway versions.

**v0.35.0** adds `GET /demo/capture/{correlationId}` on the same observer listener: a
loopback-only read-back of this gateway's own bounded, in-memory record of one transformed
egress leg's pre-seal before/after payload pair — never a wire exchange, never audited, and
never checked by any conformance surface (see `docs/CONFIGURATION.md`, "Demo-only pre-seal
edge capture"). It is populated only when the new env `SHN_DEMO_EDGE_CAPTURE`
(`engine.Config.DemoEdgeCapture`) is set, which also requires
`OBSERVER_ADDR` to be set — otherwise the flag is gated off at config load rather than
capturing into a store nothing could ever read. `POST /demo/transform`'s existing 200 and
422 response bodies also gain an additive `chain` field in v0.35.0 — the
compatibility-chain hops the run walked (or attempted), in the same shape already published
on observer events; every existing field on both responses is unchanged.

`/demo/capture/{correlationId}` and `SHN_DEMO_EDGE_CAPTURE` are test tooling, outside the
promise; consumers pin exact gateway versions.

### Observational source certification

**Evolving (v1.0):** certification evidence is module 3's (`module 3`).

PAS ingress and native POST forwarding collect source-profile evidence after dispatch.
Each supported payload is checked independently at all three PAS/DTR lines; the
certified set and nearest source line are observations only. They do not change
routing, authority, payload bytes, acceptance, response stamps or lane readiness.
The native record describes the final POST attempt, before any polling or terminal
assembly. The provider ingress record describes the bytes dispatched and relayed.
At `CONFORMANCE_ENFORCEMENT=none` no evidence is collected and no worker starts.

The app creates separate clients at each configured validator endpoint and never
invents one: per line it uses `FHIR_CERTIFY_URL_<line>` (an address for the evidence
alone, never a routing lane), then the routing lane `FHIR_VALIDATE_URL_<line>`, then the
Compose default only once it has qualified — by routing at boot or by the evidence's own
background attempts afterwards, which never change routing's lanes; until then the line's
verdict states that no lane is configured and the qualification's state, verbatim, and no
exchange waits on or dials for a qualification. With `FHIR_DEFAULT_VALIDATOR_LANES=none`
there is no Compose default: nothing is probed, and a line with neither key states that it
is not configured and that the network has no default validator lanes. From v0.57.0 a 2.1 or
2.2 address is gated too, on its own qualification by the same qualifier; a gated client
starts nothing when constructed, and its qualification starts with the certification worker
(`engine.GatedCertificationValidator.Start`), so at `CONFORMANCE_ENFORCEMENT=none` no lane is
dialed:
until it passes, the line's verdict is unavailable, "lane not qualified", and nothing is
dialed for the exchange, so a validator still warming never answers a verdict. The gate
re-qualifies after a lane stops answering: a qualified 2.1 or 2.2 lane (a default one too) that
fails at the connection (it refuses, resets or closes it, or cannot be reached) is unqualified
for certification until it passes again, "re-qualifying", and nothing is dialed meanwhile. A
lane that does not answer within the certification's 2 s while the collection is still waiting
(that certification is recorded expired, since the client's own limits are 2.5 s) is asked for
its metadata once, and re-qualifies only if that goes unanswered (v0.63.0, above). A re-qualification
starts at most once every 30 s, plus a random part of 15 s; its attempts then follow the
qualification's own schedule, which starts over with each re-qualification: each attempt
lasts up to 120 s and repeats `GET /metadata` until it answers `200` (each request waits up to
4 s, then 1 s passes before the next; a `200` that is a readable R4 CapabilityStatement of at
most 4 MiB leads to one run of the readiness corpus, any other `200` fails the attempt), and
after a failed attempt the next waits on the gateway's shared retry backoff: 15 s after the
first failure, doubling to 5 min, each wait drawn from the upper half of its step, so the
gateways a deploy restarts together do not retry together. A validator that refuses the
connection, does not resolve or answers another status therefore gets about one metadata
request per second, and one that accepts the connection but never answers about one every
5 s, in bursts of up to 120 s that grow apart to every 4.5 to 7 min. The same schedule applies
before a lane's first qualification, which starts 15 s after the evidence's start plus a random
part of 15 s for a default lane, and at once for an address. A default lane's first
qualification for certification may be routing's identical run: while routing is still
retrying the lane, the evidence posts no corpus of its own, and once routing's attempt passes on
the same endpoint, for the same line, with the same qualification and within the same 120 s,
the evidence takes that pass as its own; any difference and it runs its own. While it defers,
the line's verdict names routing's qualification: `…; default lane qualification pending
(routing qualifying)`, then `…; default lane qualification failed, retrying (routing attempt
<n>: <reason>)`. Every
re-qualification is the evidence's own run. An attempt its budget cuts off is discarded, never
resumed: a validator that restarted since says nothing by the rows it answered before (v1.0.0;
before it, attempts were 60 s and 15 s apart for an hour, then 5 min apart, and a default lane's
evidence ran its corpus beside routing's). The budget bounds an attempt's cost; it is not part of
what qualifying means, so a lane that passes the corpus has qualified at either budget. A 5xx or malformed answer marks only that
certification unavailable, and the collection's own deadline or shutdown only that
certification expired; either leaves the lane qualified. A lane restarted between certifications, with no
failure in between, is not detected. The 2.0 line is
not gated: it certifies on the gateway's own validator without this qualification, so a freshly
started 2.0 validator can still answer a certification before it has warmed. From v0.56.0,
where certification evidence is collected, `Run` also sends each certification address it was
given one request at boot, off the request path and recording no evidence, so that a new
process's first request to a validator is not a real certification's (docs/CONFIGURATION.md);
from v0.57.0 it sends only the 2.0 validator it certifies against, since a 2.1 or 2.2 address's
qualification already sends it the readiness corpus. The handler constructors do not. Embedders may
supply independent clients through `Config.CertificationValidatorsByLine`; they must
not share routing validators or qualification wrappers. `engine.NewQualifiedCertificationValidator`
gates such a client on a qualifier. The app qualifies through `engine.QualifyValidatorLaneWith`, with a
client that records the lane's certificate; `engine.QualifyValidatorLane` is the same qualification
with its own client.
From v0.57.0 neither `engine.NewGatedCertificationValidator`
nor `engine.NewQualifiedCertificationValidator` starts its qualification loop.
`engine.New` calls `Start` on each when it starts its certification worker. An embedder that
uses such a client without `engine.New` must call `Start` itself, or the lane never qualifies.
From v0.57.0 such a client, never started and not closed, records every certification as
unavailable, "certification validator unavailable: certification validator not started (call
Start)", dials nothing, and logs one `gateway: certification_lane_not_started` line (with its
`version`, `line`, `base`, `host` and `at`) at its first certification;
`engine.CertificationNotStartedReason` is that reason. `engine.New` starts every client before
its worker certifies anything, so a gateway built with it never records this.
Missing clients are recorded as unavailable. HTTP server execution failures are unavailable
rather than conclusive invalidity. From v0.57.0 an answer whose every error is terminology
the validator could not check (a code system it does not hold, the required-binding miss on
that element when the validator also says it could not expand the bound value set for want of
each system the miss names, a Bundle entry's no-match summaries on any line when every other
error on that entry is one of these, and on the 2.1 and 2.2 lines a PAS Claim entry's match of
neither Claim profile, read as the structural level reads it: the no-match summaries naming
both Claim profiles and the other profile's own cardinality minimums and maximums, attributed
to it alone; the entry must itself carry such terminology) is unavailable, naming the
code system, rather than invalid; any other error keeps it invalid. Only a valid verdict
certifies. Logs beginning `certify: ` and `leg.certified` observer details contain
JSON metadata, hashes and verdicts, without payload snapshots. External validator
issues and errors are represented only by bounded counts, byte lengths and
SHA-256 digests; diagnostic text is never retained or broadcast in evidence, except that an
unavailable verdict names a code system the validator could not check when it is an X12 code
system canonical (any other by its size and SHA-256).

Each gateway owns one worker, a 32-payload queue and a 256-record evidence ring.
Candidates have a two-second limit, collection has a six-second limit, and queued
work expires after thirty seconds. Overflow records retain metadata only; bounded
observer notifications report drops explicitly. Callbacks must return promptly and
support concurrent invocation. Completion is stored before callback delivery.

### Observer source completion

The existing opt-in, loopback-only `OBSERVER_ADDR` listener also serves
`POST /barrier`. Success is HTTP 200 with `Content-Type: application/json`,
`Cache-Control: no-store`, and
`{"protocol":1,"incarnation":"<opaque>","events":N}`. The server waits for at
most five seconds, bounded further by request cancellation. A failed or canceled
wait returns a diagnostic non-2xx response without a successful event count;
deadline failures return 504 and other completion failures return 503. Wrong
methods return 405.

`GET /health` remains immediate and adds `protocol:1` and the same nonempty
`incarnation` to its existing `events` count. It never waits or submits validation.
`GET /events` includes `X-SHN-Observer-Incarnation` before sending frames; SSE
IDs and data bytes retain their representation. Each Hub construction gets a fresh
random identity, so counts from different source instances are not interchangeable.

`Gateway.WaitObserverCompletion(ctx)` snapshots HTTP operations already entered
through `Gateway.Handler()`, awaits their return (including deferred evidence
enqueue), then snapshots accepted certification work and awaits its observer
callbacks. Operations admitted after the first snapshot do not hold that operation
cutoff; certification accepted before the second snapshot is included. Direct
`OriginateLeg` calls outside Handler and requests not yet entered are outside this
protocol. Simultaneous external traffic is not a causal attribution guarantee.
Completion proves diagnostic delivery through the source callback, not receipt by
an SSE client; clients must separately catch up to `events` in the same incarnation.

This accounting is diagnostic only: clinical responses, asynchronous certification,
routing, authority, payload bytes and readiness remain unchanged. The ordinary
app health wrapper bypasses operation tracking. `FlushCertificationForTest` retains
its accepted-queue-only semantics. `observer.Hub.HandlerWithBarrier(wait)` enables
the capability with a context-cooperative completion waiter; nil and the existing
`Handler()` retain count-only health and do not expose `/barrier`. Both forms serve
the SSE incarnation header. No observer listener is created by default.

## Access line

Every gateway writes one `gateway: access: <json>` line for each call it
answers on an ingress route or on one of module 1's origination routes, and for
each leg it receives from the network, at
every conformance level, with nothing to configure ("Access lines", below, and
[`docs/CONFIGURATION.md`](docs/CONFIGURATION.md)). Fields and closed-set values
may be added in minor releases, so decode tolerantly. The line is an evolving
surface (Versioning policy, above): a minor release may also remove or rename a
field or a value, change what one means, or narrow a closed set, with a
[`CHANGELOG.md`](CHANGELOG.md) note. It graduates once the audit outbox's
records are produced on deployed gateways, since until then its fields may still
move with them: then each of its items takes its class's tier, so a module's
field (such as `findings.count` or `stages.validateMs`) stays evolving. The first table lists
the line's fields, a field of a nested object as `object.field`. The second
lists the fields whose values are a closed set: a value the gateway cannot
classify is written as `other`.

| Item | Class | Owner |
|---|---|---|
| `time` | core | core |
| `correlationId` | core | core |
| `trace` | core | core |
| `callId` | test tooling | test tooling |
| `direction` | core | core |
| `route` | core | core |
| `exchange` | core | core |
| `operation` | core | core |
| `contractLine` | core | core |
| `answerLine` | core | core |
| `lineCarried` | core | core |
| `sender` | core | core |
| `recipient` | core | core |
| `requestCiphertextHash` | core | core |
| `responseCiphertextHash` | core | core |
| `outcome` | core | core |
| `refusal.by` | core | core |
| `refusal.rule` | core | core |
| `refusal.stage` | core | core |
| `refusal.reason` | core | core |
| `status` | core | core |
| `latencyMs` | core | core |
| `backend.status` | core | core |
| `backend.latencyMs` | core | core |
| `backend.errorClass` | core | core |
| `backend.calls` | core | core |
| `backend.resends[].reason` | core | core |
| `backend.resends[].challenge` | core | core |
| `backend.redirects` | core | core |
| `stages.unwrapMs` | core | core |
| `stages.readsMs` | core | core |
| `stages.forwardMs` | core | core |
| `stages.validateMs` | module 3 | module 3 |
| `stages.sealMs` | core | core |
| `stages.ledgerMs` | PA workstream | PA workstream |
| `stages.writeMs` | core | core |
| `network.latencyMs` | monitoring | core |
| `network.calls` | monitoring | core |
| `answerError` | core | core |
| `authorizeError` | core | core |
| `involvedOmitted` | core | core |
| `findings.count` | module 3 | module 3 |
| `findings.refused` | module 3 | module 3 |
| `findings.kinds` | module 3 | module 3 |
| `findings.deferred` | module 3 | module 3 |
| `relayEdits` | core | core |
| `ownership` | core | core |
| `builder` | core | core |
| `requestEdits` | core | core |
| `answerOwnership` | core | core |
| `answerBuilder` | core | core |
| `answerEdits` | core | core |
| `editsNotMade` | core | core |
| `answerHeadersNotCarried` | core | core |
| `readsFailed` | monitoring | core |
| `hubDelivered` | core | core |

| Item | Class | Owner |
|---|---|---|
| `direction` | core | core |
| `route` | core | core |
| `exchange` | core | core |
| `operation` | core | core |
| `outcome` | core | core |
| `refusal.by` | core | core |
| `refusal.rule` | core | core |
| `refusal.stage` | core | core |
| `backend.errorClass` | core | core |
| `answerError` | core | core |
| `authorizeError` | core | core |
| `involvedOmitted` | core | core |

### Access lines

**New in v0.58.0.**

- Every gateway writes one `gateway: access: <json>` line for each call it
  answers on a Da Vinci ingress route or for a leg it receives from the
  network, at every conformance level, with nothing to configure. The JSON is
  `diagnostics.AccessLine`, the `ExchangeRecord` below with durations in
  milliseconds (see [docs/CONFIGURATION.md](docs/CONFIGURATION.md), "Access
  lines"). It is an **evolving** surface (Access line, above): fields and
  closed-set values may be added in minor releases, so decode tolerantly, and a
  minor release may also remove or rename a field or a value, change what one
  means, or narrow a closed set, with a [`CHANGELOG.md`](CHANGELOG.md) note.
- With diagnostic collection configured, the same JSON is published as the
  `Detail` of a metadata-only event of kind `diagnostics.KindAccess`
  (`access`), which carries no body and no headers.
- A payer's gateway sends `X-Correlation-Id` with the leg's id on each
  operation it forwards to its payer's own system (not the CDS service
  listing, the connectivity probes or token requests), when the id is one
  token of letters, digits, `.`, `_` or `-`, up to 64 characters. The message
  bytes are unchanged.
- **Breaking (configuration):** `PAYER_DAVINCI_BACKEND_HEADERS` may no longer
  name `X-Correlation-Id`; a gateway configured with it refuses to boot with
  an error naming it, as for the other names the gateway sets itself. In the
  Go API, `engine.WithBackendHeaders` drops the name: the header is only ever
  the leg's id, or absent.
- **Additive setting:** `PAYER_DAVINCI_BACKEND_CORRELATION=off` (Go API:
  `engine.WithoutBackendCorrelation`) stops the header being sent, for a
  payer system that validates `X-Correlation-Id` its own way. Unset or `on`
  sends it; any other value refuses the boot.
- **Additive (v1.1.0):** access lines, and the exchange record and its
  metrics (EMF and OpenTelemetry), also cover module 1's origination routes.
  Each call a provider's gateway answers on a `/scenario` route that
  originates (builds and sends a request of its own) is recorded with
  direction `ingress` and route `scenario-*`, the route's path joined by `-`
  (`scenario-uc07-complete` for `POST /scenario/uc07/complete`): the request
  enters from the participant's own side and this gateway sends the leg, so
  `sender` is this gateway. Its `exchange` and `correlationId` are the first
  leg's the call sent (`other` and absent when it sent none), while a refusal
  it records may belong to a later leg of the same call; a refusal carries
  its rule as on an ingress call: the Hub's or a peer's, `routing` for a
  coverage that names no payer the network registers, `authentication` for a
  refused `/scenario` caller, `fidelity` for claim facts stated for an order
  the member does not hold. Its `latencyMs` is the whole call, a flow of legs,
  so it feeds the exchange count and refusals but not `ExchangeLatency`, the
  exchange duration and stage instruments or `/internal/stats` latency. The
  local `/scenario` routes (`uc06/cancel`, `uc07/cancel`, `uc07/pending`,
  `reset`) send nothing and are not recorded. In the Go API,
  `engine.ExchangeOriginationRoutes` (additive) names the recorded routes.
- **Additive (v1.1.0):** a fidelity refusal's access line carries
  `refusal.stage`, and its record `RefusalStage` (`recording.RefusalStages`):
  `build`, SHN refused to build the message the refusal is about, because of
  what the caller declared or did not state (earlier legs of the same call
  may already have been sent), or `verify`, an edit or message the gateway
  built failed its own verification. Absent for every other rule.
- **Additive (v1.7.0):** a refusal the gateway makes of its own participant's
  call before the request it refuses reaches the Hub carries `refusal.reason`
  on its access line, and `RefusalReason` on its record (on a module's
  origination route, an earlier leg of the same call may have been sent). This covers refusals for
  authentication or scope at an ingress, message integrity, authority read
  from the body, routing with no recipient, and size.
  - **Value:** the gateway's own fixed refusal sentence: the answer its caller
    is given, or the fixed sentence in it when the answer adds a detail (an
    identifier, a member name, a count, the contract lines). It is never a value from the request,
    a credential or patient data. A value the gateway has not registered is
    written as `other`.
  - **Stability:** the field is core; its sentences are not. A minor release
    may reword a sentence or add one, so decide by `refusal.rule`, never by
    the text.
  - **Absent:** on a refusal by the Hub, a peer or the Authorization Framework,
    on a leg received from the network, and on a refusal of an answer that came
    back from the network.

### One record per exchange

**New in v0.58.0.**

- A record is metadata only: the leg's correlation id, the caller's own
  `X-Correlation-Id` when it differs, the pa-test door's verified call id, the
  leg, the operation, the contract line, the two holders, the outcome and who
  refused on which network rule, the status, the latency, a count of
  conformance findings, and, on the answering side, the call to the
  participant's own system (its status, latency and error class). It never
  carries a message body or a patient identifier. The only header values it
  carries are the caller's own `X-Correlation-Id`, which the ingress accepts
  only as one bounded token, the call id of a verified `X-SHN-Test-Trace`
  proof, and, from v0.61.0, the Hub's `X-SHN-Delivered` on its error answer
  to a leg (`HubDelivered`, only as `no`, `yes` or `unknown`); a leg id a message chose (a PAS Claim's own `urn:shn:correlation`) is
  recorded as `sha256:<digest>` unless it is one bounded token too.
- It also carries the sha256 of the request and answer envelopes'
  ciphertext, the value the Hub's audit records carry as `payloadBundleHash`,
  so a record joins the Hub's records for the same leg.
- From v0.61.0 it names the registered edits (`Edits`: `E-01` …) the gateway
  applied to the bytes it transmitted on the leg, in transmit order: ids only,
  each once, only for bytes sent (the receiver answered, or the connection
  failed after the request was written). Not a metric dimension.
- Direction, route, exchange, operation, outcome, refusing party, rule,
  refusal stage and backend error class are closed sets
  (`engine.ExchangeDirections`, `ExchangeRoutes`, `ExchangeKinds`,
  `ExchangeOperations`, `ExchangeOutcomes`, `RefusalParties`, `RefusalRules`,
  `BackendErrorClasses`, and `recording.RefusalStages`), each ending in
  `other`: a value the gateway cannot classify is recorded as `other`, never as
  a caller's string.
- **Fixed in v0.59.0:** a payer that requires known members
  (`REQUIRE_KNOWN_MEMBERS=true`) records its `400 unknown member` refusal on
  `conformance`, the rule for a check a participant opted into. v0.58.0
  recorded it on `other`.
- A requester's gateway cannot tell a recipient gateway's refusal inside the
  answer's frame from the recipient's own system answering an error, so its
  record calls either one `upstream-error`; the answering gateway's record
  says which it was. A refusal the recipient's gateway gave at its edge, which
  the Hub reports, is `refused` by `payer-gateway`.
- On the answering side, a refusal of this gateway's own that names no rule,
  given because its participant's system did not answer usably (a service
  listing it could not read, or an error answer a requester without a message
  frame cannot be sent), is recorded as `upstream-error`, with the failed call;
  so is a read or search of its system of record that failed, except, from
  v1.1.0, one the gateway knows repeats a member name: a search page, or a
  read the connector reports with `engine.SoRRepeatedMember` or
  `engine.SoRInvalidAnswer` (the bundled FHIR connector reports every answer it
  reads that repeats a member name, exactly or under case folding, and acts on
  no reading of it). That answer can be read two ways, so
  it is `refused` by this gateway on `integrity`, the call `malformed`, as the
  participant's answer to a forwarded operation is when it repeats a member
  name; v1.0.0 recorded it `upstream-error` (on the federated query, coverage
  eligibility, patient-authored DTR and the member binding of a leg it
  answers). Every read and
  search of the participant's system of record on a leg it answers is a
  backend call (status 0, with its latency), and so is a read of its CDS
  service listing. `Backend` is the call the answer came from: the operation
  forwarded to the participant's system, or, where none was, the last read; a
  read after the operation (a check of its answer) is counted but does not
  replace it. An answer is `malformed` only when this gateway cannot read it;
  a readable answer a check found fault with keeps no class. At `none`, where
  no conformance check runs, an answer is `malformed` only when the gateway
  itself could not read it: one repeating a member name (the network's own
  rule), the CDS service listing, a system-of-record read the connector
  reports invalid (or fails without saying why), a search page, or a
  facility's records out of shape (another patient's record, no Patient, a
  record without an id or the same record twice). A call cut short because
  the request it served ended has error class `cancelled`, and one that ran
  out of time `timeout`, whatever the participant's connector reported; a
  call or read abandoned while the request was still live (a connector
  cancelling its own read, a client its own attempt) is the system not
  answering. A forwarded operation that runs past the responder's own
  deadline (`engine.WithBackendDeadline`, which the gateway sets from
  `PAYER_DAVINCI_BACKEND_TIMEOUT`) is the participant's system's `timeout`,
  and its exchange that system's upstream error. The exchange of a cut-short call is
  never an upstream error: a refusal it caused, the operation's or a later
  read's, is `other`. A system-of-record read that a
  check at `strict` needed and could not make refuses as that check's
  `conformance` refusal; the read is counted in `BackendCalls`, and, after
  the operation, is not the `Backend`.
- `limit` is recorded for a coverage eligibility batch of more requests than
  a gateway reads in one batch (from v0.63.0); from v0.58.0 the gateway records an Authorization
  Framework denial as `authority`, whatever its reason; a facility's `consent`
  refusals (a federated query without a consent reference, or one the consent
  service does not permit) are `consent`. A facility's or a PHG's gateway
  refusing a leg is refused by `other`.

## Metrics

With `METRICS_SERVICE` set, the gateway writes metrics to standard output in
CloudWatch embedded metric format, in the namespace `METRICS_NAMESPACE` with
the `Env` dimension from `METRICS_ENV` (see
[`docs/CONFIGURATION.md`](docs/CONFIGURATION.md), "Exchange metrics"); without
it, it writes none. Apart from `Service` and `Env`, which are your own values,
every dimension value comes from a fixed list; values may be added in minor
releases. A value that names an evolving leg (the `exchange` dimension's
`coverage-eligibility`, `patient-dtr` and `federated-query`) follows its leg's
tier: a minor release may remove or rename it with its leg.

| Item | Class | Owner |
|---|---|---|
| `LegOutcome` | core | core |
| `LegError` | core | core |
| `Exchange` | core | core |
| `ExchangeLatency` | core | core |
| `BackendCall` | core | core |
| `BackendLatency` | core | core |
| `BackendError` | core | core |
| `StoreError` | core | core |
| `InvolvedOmitted` | core | core |
| `StorePool` | core | core |
| `AuditAppendSent` | core | core |
| `AuditAppendRetried` | core | core |
| `AuditAppendDroppedBy` | core | core |
| `AuditAppendDropped` | core | core |
| `AuditQueueDepth` | core | core |
| `AuditEndpointUnknown` | core | core |
| `ValidatorLane` | core | core |
| `ValidatorLaneAttempt` | core | core |
| `GatewayInfo` | monitoring | core |
| `CertificateExpiry` | monitoring | core |
| `CertificateExpiryMin` | monitoring | core |
| `IngressKeyRemaining` | monitoring | core |
| `IngressKeyAge` | monitoring | core |
| `DependencyAge` | monitoring | core |
| `DependencyAgeMax` | monitoring | core |
| `PeersKnown` | monitoring | core |
| `TokenLatency` | monitoring | core |
| `TokenAcquisition` | monitoring | core |
| `ExchangeRefused` | monitoring | core |
| `ExchangeRefusedByRule` | monitoring | core |
| `ExchangeStageLatency` | monitoring | core |
| `ActiveRequests` | monitoring | core |
| `RequestBytes` | monitoring | core |
| `ResponseBytes` | monitoring | core |
| `ConformanceFinding` | monitoring | core |
| `ValidatorLatency` | monitoring | core |
| `ObserveCheckDropped` | monitoring | core |
| `ListenerCut` | monitoring | core |

## OpenTelemetry instruments

**Evolving (v1.0):** every item in the first two tables below.

With OpenTelemetry export on (`OTEL_METRICS_EXPORTER`; see
[`docs/CONFIGURATION.md`](docs/CONFIGURATION.md), "OpenTelemetry metrics
export"), the gateway exports the instruments in the first table, over OTLP
push or its Prometheus pull listener, with the attributes in the second: its
own, then the Go runtime's, which the OpenTelemetry runtime instrumentation
records (`go.*`), and the process's, which the gateway records as the
semantic conventions define them (`process.*`). The
EMF metrics above are unchanged by it, and both can be on at once. Prometheus
names follow from these by Prometheus' OpenTelemetry translation with
underscore escaping and suffixes: dots become underscores, a
monotonic counter gains `_total` (an up-down counter does not), and a unit in
seconds, bytes or percent gains `_seconds`, `_bytes` or `_percent`
(`shn.gateway.backend.calls` is `shn_gateway_backend_calls_total`,
`shn.gateway.exchange.duration` is `shn_gateway_exchange_duration_seconds`,
and the attribute `shn.backend.class` is the label `shn_backend_class`). The
names here are the promise; the translated names follow from them.

Every attribute value comes from a fixed list; values may be added in minor
releases. A value outside its list is recorded as `other`, except on
`http.server.request.duration`, whose attributes are the OpenTelemetry
semantic convention's and record such a value as the convention's `_OTHER`,
and on the runtime and process instruments, whose attributes and values
(`go.memory.type`, `cpu.mode`) are the semantic conventions'.
Three of those are absent where they do not apply, as the convention says:
`http.response.status_code` when a handler panicked before writing a status,
`http.route` when no route of the gateway's matched, and `error.type` when the
request did not fail. No attribute carries a participant, an identifier, a URL
or free text. A value that names an evolving leg follows its leg's tier, as on
the EMF `exchange` dimension.

| Item | Class | Owner |
|---|---|---|
| `shn.gateway.exchanges` | monitoring | core |
| `shn.gateway.exchange.duration` | monitoring | core |
| `shn.gateway.backend.calls` | monitoring | core |
| `shn.gateway.backend.duration` | monitoring | core |
| `shn.gateway.legs` | monitoring | core |
| `shn.gateway.store.errors` | monitoring | core |
| `shn.gateway.involved.omitted` | monitoring | core |
| `shn.gateway.store.pool.connections` | monitoring | core |
| `shn.gateway.store.pool.empty_acquires` | monitoring | core |
| `shn.gateway.audit.appends` | monitoring | core |
| `shn.gateway.audit.retries` | monitoring | core |
| `shn.gateway.audit.drops` | monitoring | core |
| `shn.gateway.audit.queue.depth` | monitoring | core |
| `shn.gateway.audit.endpoint.unknown` | monitoring | core |
| `shn.gateway.validator.lane.qualified` | monitoring | core |
| `shn.gateway.validator.lane.attempts` | monitoring | core |
| `shn.gateway.info` | monitoring | core |
| `shn.gateway.certificate.expiry` | monitoring | core |
| `shn.gateway.ingress_key.remaining` | monitoring | core |
| `shn.gateway.ingress_key.age` | monitoring | core |
| `shn.gateway.dependency.age` | monitoring | core |
| `shn.gateway.peers.known` | monitoring | core |
| `shn.gateway.authorization.token.duration` | monitoring | core |
| `shn.gateway.authorization.tokens` | monitoring | core |
| `shn.gateway.telemetry.exports` | monitoring | core |
| `shn.gateway.refusals` | monitoring | core |
| `shn.gateway.exchange.stage.duration` | monitoring | core |
| `shn.gateway.active_requests` | monitoring | core |
| `shn.gateway.exchange.request.size` | monitoring | core |
| `shn.gateway.exchange.response.size` | monitoring | core |
| `shn.gateway.conformance.findings` | monitoring | core |
| `shn.gateway.validator.duration` | monitoring | core |
| `shn.gateway.conformance.checks.dropped` | monitoring | core |
| `shn.gateway.listener.cuts` | monitoring | core |
| `http.server.request.duration` | monitoring | core |
| `go.config.gogc` | monitoring | core |
| `go.goroutine.count` | monitoring | core |
| `go.memory.allocated` | monitoring | core |
| `go.memory.allocations` | monitoring | core |
| `go.memory.gc.goal` | monitoring | core |
| `go.memory.limit` | monitoring | core |
| `go.memory.used` | monitoring | core |
| `go.processor.limit` | monitoring | core |
| `process.cpu.time` | monitoring | core |
| `process.memory.usage` | monitoring | core |
| `process.uptime` | monitoring | core |

| Item | Class | Owner |
|---|---|---|
| `shn.direction` | monitoring | core |
| `shn.exchange` | monitoring | core |
| `shn.outcome` | monitoring | core |
| `shn.backend.class` | monitoring | core |
| `shn.store` | monitoring | core |
| `shn.reason` | monitoring | core |
| `shn.pool.state` | monitoring | core |
| `shn.ig_line` | monitoring | core |
| `shn.lane` | monitoring | core |
| `shn.wire_protocol` | monitoring | core |
| `shn.conformance.level` | monitoring | core |
| `shn.endpoint.kind` | monitoring | core |
| `shn.cert.role` | monitoring | core |
| `shn.dependency` | monitoring | core |
| `shn.refusal.rule` | monitoring | core |
| `shn.refusal.by` | monitoring | core |
| `shn.refusal.stage` | monitoring | core |
| `shn.stage` | monitoring | core |
| `shn.finding.kind` | monitoring | core |
| `shn.finding.action` | monitoring | core |
| `shn.finding.tolerated` | monitoring | core |
| `shn.listener` | monitoring | core |
| `shn.listener.bound` | monitoring | core |
| `http.request.method` | monitoring | core |
| `http.response.status_code` | monitoring | core |
| `http.route` | monitoring | core |
| `url.scheme` | monitoring | core |
| `network.protocol.version` | monitoring | core |
| `error.type` | monitoring | core |
| `cpu.mode` | monitoring | core |
| `go.memory.type` | monitoring | core |

The examples in [`deploy/monitoring/`](deploy/monitoring/README.md), a
Grafana dashboard, Prometheus alert rules and OpenTelemetry Collector recipes,
are starting points outside the promise: they may change in any release. The
metric names and attributes they read are the items above.

| Item | Class | Owner |
|---|---|---|
| `deploy/monitoring/**` | unsupported | core |

## Health and checks

`GET /health` on the main listener reports the gateway's health, and the
observer listener serves a `GET /health` of its own. `GET` and
`POST /internal/checks` report and run the gateway's checks of the services it
depends on (behind `CHECKS_TOKEN`, or from loopback only). Fields, check names
and failure codes may be added in minor releases, so decode tolerantly and
never reject an unknown field. The table lists the fields of the main
listener's `/health` payload, then the observer listener's `/health` payload as
one row, then the `/internal/checks` answer's `checkedAt` and the fields of
each of its `results`, with the values of `results[].failure.code` one row each.

| Item | Class | Owner |
|---|---|---|
| `service` | core | core |
| `version` | SHN-own | SHN Cloud |
| `uptimeSeconds` | core | core |
| `status` | core | core |
| `checks[].name` | core | core |
| `checks[].status` | core | core |
| `checks[].lastSuccess` | core | core |
| `checks[].lastError` | core | core |
| `checks[].holderCount` | core | core |
| `observer /health` | core | core |
| `checkedAt` | core | core |
| `results[].id` | core | core |
| `results[].target` | core | core |
| `results[].ok` | core | core |
| `results[].detail` | core | core |
| `results[].checkedAt` | core | core |
| `results[].latencyMs` | core | core |
| `results[].failure.code=unreachable` | core | core |
| `results[].failure.code=http-status` | core | core |
| `results[].failure.code=invalid-capability-statement` | core | core |
| `results[].failure.code=credential-rejected` | core | core |
| `results[].failure.code=not-checked` | core | core |
| `results[].failure.code=internal` | core | core |
| `results[].failure.code=version-drift` | core | core |
| `results[].failure.code=lane-unqualified` | core | core |
| `results[].failure.code=invalid-davinci-configuration` | PA workstream | PA workstream |
| `results[].failure.code=invalid-cds-services` | PA workstream | PA workstream |
| `results[].failure.code=endpoint-unknown` | core | core |
| `results[].failure.code=invalid-telemetry-configuration` | monitoring | core |
| `results[].failure.hint` | core | core |
| `results[].capability.fhirVersion` | core | core |
| `results[].capability.implementationGuides` | core | core |
| `results[].capability.supportedProfiles` | core | core |
| `results[].capability.contractVersions` | core | core |
| `results[].capability.declaredContractVersions` | core | core |
| `results[].capability.endpointURLs` | core | core |
| `results[].anonymous.ok` | core | core |
| `results[].anonymous.detail` | core | core |
| `results[].anonymous.failure` | core | core |
| `results[].advisory` | monitoring | core |
| `results[].noProbe` | monitoring | core |

### `GET /health`

**`GET /health`** (served by the `app` runner in front of the engine handler): the shared
SHN health payload — `service` (the gateway's holder id), `version` (`SHN_VERSION`
when set, else the build's own version: a release's tag, a preview build's pseudo-version, the
module version Go recorded for an unstamped build, or `v0.0.0-unstamped` for one that states
none), `uptimeSeconds`, a worst-check-wins `status`, and a `checks`
array (`registrar-poller` and, from v1.1.0, `capability-declaration` when a
registrar feed is configured; `store` when the durable Postgres store is
configured). The payload shape is the published
`shn-sdk/health` contract (v0.29.0) and is non-sensitive by construction —
statuses, timestamps, counts, and coarse error classes only. The JSON field
shape is part of the binary contract. Which checks a gateway reports, and so
their names, follows its own configuration (a registrar feed, a durable store),
not a fixed list. The `shn-sdk/health` Go package that defines the payload is
SHN-internal.

### `GET`/`POST /internal/checks` results

**`GET`/`POST /internal/checks` results** (since v0.32.0; structured
`failure` since v0.33.0). Each result is `{id, target, ok, detail, checkedAt,
latencyMs}` plus, on failing results only, `failure {code, hint}` with `code` drawn
from a closed set (`unreachable`, `http-status`, `invalid-capability-statement`,
`credential-rejected`, `not-checked`, `internal`, and the codes the table above
adds). From v1.0.0, the `AUDIT_ENDPOINT` result checks the audit endpoint the
network's discovery names (`endpoints.audit`), and fails `endpoint-unknown`,
probing nothing, when it names none. From v0.59.0, a `$metadata` check of
a base configured with a credential reads authenticated and adds an informational
`anonymous {ok, detail, failure}` object, the same read without the credential, which
never decides `ok`. The result shape and the closed set of failure codes are part of
the binary contract: new keys and codes may appear in minor releases, so decode
tolerantly, never with unknown-field rejection. Which results a gateway reports, and
their ids, follows its own configuration (its lanes and the endpoints it depends on),
not a fixed list. See `docs/CONFIGURATION.md`
("Operational checks") for semantics and redaction guarantees.

## Protocol

The protocol between a gateway, the Hub, the Authorization Framework, the
registrar and the network's discovery document is specified in the SHN
Participant Protocol, published with `shn-sdk`, and versioned by its own wire
protocol version (`wireProtocolVersion`). A gateway release promises to speak
the wire protocol versions it declares; a new wire protocol version is that
versioning at work, not a break of the gateway's contract. The table lists the items the
gateway sends, reads or serves: the Hub request and its envelope's fields, the
leg types an envelope's `transactionType` names, the authorization request, the
protocol headers, the message frame's headers (a frame's version, `v1`, or
`v1op` for a request frame that carries a DTR operation, is named in a
holder's `messageFrames` and `requestFrames`), the network discovery
document's fields, the registrar's holder fields the gateway reads, what its ingress serves to a
participant's own systems (the bearer token, its scope, and the endpoint codes
and CDS service ids its discovery documents name), and the headers and
endpoints of optional diagnostic collection.

Of the leg types, `coverage-eligibility` and `patient-dtr` (`PA workstream,
evolving`) and `federated-query` (`FQS workstream`) are evolving at v1.0, and
the CDex legs (`PA workstream, evolving`, from v1.1.0 `cdex-attachment-submit`
and from v1.5.0 `cdex-attachment-request`) are evolving when they ship: a minor gateway release may change how the gateway
carries them. The wire protocol's own versioning still governs their names.

| Item | Class | Owner |
|---|---|---|
| `POST <hub>/route` | core | core |
| `POST <audit>/append` | core | core |
| `X-Holder-Assertion` | core | core |
| `metadata` | core | core |
| `ciphertext` | core | core |
| `metadata.sender` | core | core |
| `metadata.recipient` | core | core |
| `metadata.transactionType` | core | core |
| `metadata.authorityFrame` | core | core |
| `metadata.consentRef` | core | core |
| `metadata.authzToken` | core | core |
| `metadata.timestamp` | core | core |
| `metadata.correlationId` | core | core |
| `metadata.involved` | core | core |
| `transactionType=coverage-eligibility` | PA workstream, evolving | PA workstream |
| `transactionType=crd-order-select` | PA workstream | PA workstream |
| `transactionType=crd-order-dispatch` | PA workstream | PA workstream |
| `transactionType=dtr-questionnaire-fetch` | PA workstream | PA workstream |
| `transactionType=pas-claim` | PA workstream | PA workstream |
| `transactionType=pas-claim-update` | PA workstream | PA workstream |
| `transactionType=pas-claim-inquire` | PA workstream | PA workstream |
| `transactionType=patient-dtr` | PA workstream, evolving | PA workstream |
| `transactionType=cdex-attachment-submit` | PA workstream, evolving | PA workstream |
| `transactionType=cdex-attachment-request` | PA workstream, evolving | PA workstream |
| `transactionType=federated-query` | FQS workstream | FQS workstream |
| `POST <authz>/authorize` | core | core |
| `X-Hub-Assertion` | core | core |
| `X-SHN-Delivered` | core | core |
| `X-SHN-Not-Acted` | core | core |
| `X-SHN-Leg-Id` | core | core |
| `X-SHN-Edited` | core | core |
| `X-Correlation-Id` | core | core |
| `SHN-Builder` | module 1 | module 1 |
| `X-SHN-Observer-Incarnation` | core | core |
| `Content-Type` | core | core |
| `contractVersion` | core | core |
| `operation` | core | core |
| `operation=questionnaire-package` | PA workstream | PA workstream |
| `operation=next-question` | PA workstream | PA workstream |
| `wireProtocolVersion` | core | core |
| `wireProtocolVersionsAccepted` | core | core |
| `authzPublicKeyURL` | core | core |
| `hubTransportKeyURL` | core | core |
| `endpoints.authz` | core | core |
| `endpoints.hub` | core | core |
| `endpoints.registrar` | core | core |
| `endpoints.consent` | core | core |
| `endpoints.audit` | core | core |
| `endpoints.phg` | SHN-own | PHG |
| `fhirValidateURL` | core | core |
| `hubAccepts` | core | core |
| `GET <registrar>/holders` | core | core |
| `id` | core | core |
| `role` | core | core |
| `encPub` | core | core |
| `signPub` | core | core |
| `baseURL` | core | core |
| `payerIds` | core | core |
| `messageFrames` | core | core |
| `contractVersions` | core | core |
| `requestFrames` | core | core |
| `Authorization: Bearer` | core | core |
| `system/Davinci.write` | PA workstream | PA workstream |
| `davinci_crd_hook_endpoint` | PA workstream | PA workstream |
| `davinci_dtr_qpackage_endpoint` | PA workstream | PA workstream |
| `davinci_pas_submission_endpoint` | PA workstream | PA workstream |
| `shn-order-sign` | PA workstream | PA workstream |
| `shn-order-select` | PA workstream | PA workstream |
| `shn-order-dispatch` | PA workstream | PA workstream |
| `pa.pdex` | unsupported | core |
| `X-SHN-Test-Trace` | test tooling | test tooling |
| `X-SHN-Evidence-Source` | test tooling | test tooling |
| `X-SHN-Evidence-Incarnation` | test tooling | test tooling |
| `X-SHN-Evidence-Time` | test tooling | test tooling |
| `X-SHN-Evidence-Signature` | test tooling | test tooling |
| `X-SHN-Evidence-Batch` | test tooling | test tooling |
| `<sink>` | test tooling | test tooling |
| `<sink>/batch` | test tooling | test tooling |
| `<sink-parent>/health` | test tooling | test tooling |

### Cross-version conformance contract

The **`shn-sdk` wire vectors** and the **SHN Participant Protocol
specification** (published with `shn-sdk`) are the conformance contract across
gateway versions. A gateway that passes the wire-vector suite is conformant
with the SHN exchange protocol regardless of the gateway version it runs.

## Image

The gateway is published as a container image built from the release's public
source tag. The image runs the single static binary `/gateway` as its
entrypoint, with no arguments, as the distroless non-root user `65532:65532`;
there is no shell, so a health check must be a binary or none. The image
declares no `EXPOSE`, `HEALTHCHECK`, `ENV` or `VOLUME`, and the table lists
them because their absence is part of the contract: the listener's port comes
from `PORT`, and you mount your key bundle where you choose and name it in
`SHN_SECRETS`. The `/etc/shn` path the image's build file mentions is a
convention, not part of the image contract. From v1.0.0 the image is an index
for `linux/amd64` and `linux/arm64`, each image with an attested SPDX SBOM;
before it, one `linux/amd64` image. Images are signed keyless with cosign;
[`README.md`](README.md) gives the certificate identity and OIDC issuer to
verify a signature with. The `vX.Y.Z-rN` tags are
SHN Cloud's own copies for hosted gateways.

`/gateway -version`, the one argument the binary takes, prints the build's version and where it
came from (`v1.0.0 stamp`, or `v0.0.0-unstamped unstamped` for a build that states none) and
exits 0. It reads no setting, file or network first, so it answers with an empty environment.

| Item | Class | Owner |
|---|---|---|
| `ENTRYPOINT ["/gateway"]` | core | core |
| `/gateway -version` | core | core |
| `USER 65532:65532` | core | core |
| `gcr.io/distroless/static-debian12:nonroot` | core | core |
| `EXPOSE` | core | core |
| `HEALTHCHECK` | core | core |
| `ENV` | core | core |
| `VOLUME` | core | core |
| `/etc/shn` | unsupported | core |
| `ghcr.io/smarthealthnetwork/shn-gateway:vX.Y.Z` | core | core |
| `docker.io/<namespace>/shn-gateway:vX.Y.Z` | core | core |
| `vX.Y.Z-rN` | SHN-own | SHN Cloud |
| `linux/amd64` | core | core |
| `linux/arm64` | core | core |
| `cosign keyless signature` | core | core |
| `SPDX SBOM` | core | core |

## The `shn` CLI

The `shn` CLI is published with `shn-sdk`, so its versions are `shn-sdk`'s. From
`shn-sdk` v1.0.0 the promise covers the commands a participant onboards with:
`keygen`, `register`, `login`, `clients`, `revoke`, `rotate` and `doctor`, and
`shn` run with no command, an unknown command or `help`. For each of them it
covers the command's name, its flags, its exit codes and the files `keygen` and
`register` write. `gateway stats` is in the evolving `monitoring` class, as
described below. What a command prints is outside the promise.

`eligibility`, `priorauth`, `priorauth resume`, `send-test`, `scenario-key` and
`scenario-sign` drive exchanges against the operator-run test responders and a
gateway's `/scenario` routes. They are test tooling: outside the promise, so a
minor release may change or remove them, their flags or their exit codes. Where
the participant documentation uses them, its examples show what the command does
today, not a promise.

`gateway stats` reads `GET /internal/stats` from one or more replicas of a Smart
Gateway (`--url`, repeatable, each a replica's base URL) and merges their answers
into one table by exchange, adding the replicas' latency sketches so its
quantiles are those of all their traffic together. It is in the `monitoring`
class: evolving, so until a minor release declares it graduated, a minor
release may change it or its flags. It exits 0 with the merged table, 1 when a
replica cannot be read or its answer cannot be merged exactly (a malformed
answer, an answer without the window asked for, sketches bucketed differently,
or one replica given twice), and 2 on a usage error (among them a `--window`
other than `1m`, `5m` or `1h`, and `--token-file` with a plain `http://` URL
that is not this host's); `shn gateway` without `stats` exits 2.

The first table lists the commands, the second their flags (one row per flag,
written `shn <command> --<flag>`), the third the exit codes of each promised
command (`shn <command> exit <code>`) and the fourth the files a key directory
holds.

| Item | Class | Owner |
|---|---|---|
| `shn` | core | core |
| `shn help` | core | core |
| `shn -h` | core | core |
| `shn --help` | core | core |
| `shn keygen` | core | core |
| `shn register` | core | core |
| `shn login` | core | core |
| `shn clients` | core | core |
| `shn revoke` | core | core |
| `shn rotate` | core | core |
| `shn doctor` | core | core |
| `shn eligibility` | test tooling | test tooling |
| `shn priorauth` | test tooling | test tooling |
| `shn priorauth resume` | test tooling | test tooling |
| `shn send-test` | test tooling | test tooling |
| `shn scenario-key` | test tooling | test tooling |
| `shn scenario-sign` | test tooling | test tooling |
| `shn gateway` | monitoring | core |
| `shn gateway stats` | monitoring | core |

`register` has two forms. With `--accounts` it registers through the Accounts
service; its flags are the rows `shn register --accounts --<flag>`. Without it,
it is the operator's direct registration with the registrar; its flags are the
rows `shn register --<flag>`. The operator form reads `--portal` only to refuse
with exit code 2, naming `--accounts` in its place; the `--accounts` form does
not know `--portal` and exits 2 as for any unknown flag.

| Item | Class | Owner |
|---|---|---|
| `shn keygen --base-url` | core | core |
| `shn keygen --id` | core | core |
| `shn keygen --out` | core | core |
| `shn keygen --role` | core | core |
| `shn register --admin-assertion` | core | core |
| `shn register --base-url` | core | core |
| `shn register --contract-versions` | core | core |
| `shn register --name` | core | core |
| `shn register --out` | core | core |
| `shn register --portal` | unsupported | core |
| `shn register --registrar` | core | core |
| `shn register --request-frames` | core | core |
| `shn register --role` | core | core |
| `shn register --accounts` | core | core |
| `shn register --accounts --base-url` | core | core |
| `shn register --accounts --cache` | core | core |
| `shn register --accounts --contract-versions` | core | core |
| `shn register --accounts --name` | core | core |
| `shn register --accounts --out` | core | core |
| `shn register --accounts --payer-id` | core | core |
| `shn register --accounts --request-frames` | core | core |
| `shn register --accounts --role` | core | core |
| `shn login --accounts` | core | core |
| `shn login --cache` | core | core |
| `shn login --no-browser` | core | core |
| `shn clients --accounts` | core | core |
| `shn clients --cache` | core | core |
| `shn revoke --accounts` | core | core |
| `shn revoke --cache` | core | core |
| `shn rotate --contract-versions` | core | core |
| `shn rotate --out` | core | core |
| `shn rotate --registrar` | core | core |
| `shn rotate --request-frames` | core | core |
| `shn doctor --discovery` | core | core |
| `shn doctor --id` | core | core |
| `shn doctor --keys` | core | core |
| `shn doctor --out` | core | core |
| `shn doctor --persona` | core | core |
| `shn eligibility --authz` | test tooling | test tooling |
| `shn eligibility --authz-pub` | test tooling | test tooling |
| `shn eligibility --dob` | test tooling | test tooling |
| `shn eligibility --family` | test tooling | test tooling |
| `shn eligibility --hub` | test tooling | test tooling |
| `shn eligibility --member` | test tooling | test tooling |
| `shn eligibility --name` | test tooling | test tooling |
| `shn eligibility --npi` | test tooling | test tooling |
| `shn eligibility --out` | test tooling | test tooling |
| `shn eligibility --payer-enc` | test tooling | test tooling |
| `shn eligibility --payer-id` | test tooling | test tooling |
| `shn eligibility --payer-identifier` | test tooling | test tooling |
| `shn priorauth --claim-type` | test tooling | test tooling |
| `shn priorauth --discovery` | test tooling | test tooling |
| `shn priorauth --id` | test tooling | test tooling |
| `shn priorauth --keys` | test tooling | test tooling |
| `shn priorauth --member` | test tooling | test tooling |
| `shn priorauth --out` | test tooling | test tooling |
| `shn priorauth --place-of-service` | test tooling | test tooling |
| `shn priorauth --priority` | test tooling | test tooling |
| `shn priorauth --request-type` | test tooling | test tooling |
| `shn priorauth --resume-out` | test tooling | test tooling |
| `shn priorauth --service-category` | test tooling | test tooling |
| `shn priorauth resume --discovery` | test tooling | test tooling |
| `shn priorauth resume --id` | test tooling | test tooling |
| `shn priorauth resume --keys` | test tooling | test tooling |
| `shn priorauth resume --out` | test tooling | test tooling |
| `shn priorauth resume --provenance-agent-system` | test tooling | test tooling |
| `shn priorauth resume --provenance-agent-value` | test tooling | test tooling |
| `shn priorauth resume --report-file` | test tooling | test tooling |
| `shn priorauth resume --resume` | test tooling | test tooling |
| `shn send-test --bundle` | test tooling | test tooling |
| `shn send-test --discovery` | test tooling | test tooling |
| `shn send-test --follow` | test tooling | test tooling |
| `shn send-test --gateway` | test tooling | test tooling |
| `shn send-test --gateway-id` | test tooling | test tooling |
| `shn send-test --json` | test tooling | test tooling |
| `shn send-test --label` | test tooling | test tooling |
| `shn send-test --scenario-key` | test tooling | test tooling |
| `shn scenario-key --bundle` | test tooling | test tooling |
| `shn scenario-key --callers-file` | test tooling | test tooling |
| `shn scenario-key --force` | test tooling | test tooling |
| `shn scenario-key --label` | test tooling | test tooling |
| `shn scenario-key --out` | test tooling | test tooling |
| `shn scenario-sign --body-file` | test tooling | test tooling |
| `shn scenario-sign --bundle` | test tooling | test tooling |
| `shn scenario-sign --gateway-id` | test tooling | test tooling |
| `shn scenario-sign --key` | test tooling | test tooling |
| `shn scenario-sign --label` | test tooling | test tooling |
| `shn gateway stats --json` | monitoring | core |
| `shn gateway stats --token-file` | monitoring | core |
| `shn gateway stats --url` | monitoring | core |
| `shn gateway stats --window` | monitoring | core |

`revoke` takes the id of the client to revoke, and `rotate` the id of the holder
whose keys it rotates, as its first argument, before the flags.

Exit code 0 is success. For `keygen`, `register`, `login`, `clients`, `revoke`
and `rotate`, 1 is a failure and 2 a usage error: a flag unknown, missing or
malformed, or the id argument missing. `doctor`'s codes name the phase that
failed, so a script can tell whose problem it is: 2 usage (a flag unknown,
missing or malformed, or `--persona` naming no persona the network
advertises); 10 the network's health,
or an eligibility or prior-authorization exchange that failed; 20 a wire version
this CLI does not speak; 30 your registration; 40 an outcome that differs from
what the network states for the persona, or a persona the network describes
incompletely. `shn` with no command or an unknown one prints its usage to
standard error and exits 2; `shn help`, `-h` and `--help` print it to standard
output and exit 0.

| Item | Class | Owner |
|---|---|---|
| `shn exit 2` | core | core |
| `shn help exit 0` | core | core |
| `shn -h exit 0` | core | core |
| `shn --help exit 0` | core | core |
| `shn keygen exit 0` | core | core |
| `shn keygen exit 1` | core | core |
| `shn keygen exit 2` | core | core |
| `shn register exit 0` | core | core |
| `shn register exit 1` | core | core |
| `shn register exit 2` | core | core |
| `shn login exit 0` | core | core |
| `shn login exit 1` | core | core |
| `shn login exit 2` | core | core |
| `shn clients exit 0` | core | core |
| `shn clients exit 1` | core | core |
| `shn clients exit 2` | core | core |
| `shn revoke exit 0` | core | core |
| `shn revoke exit 1` | core | core |
| `shn revoke exit 2` | core | core |
| `shn rotate exit 0` | core | core |
| `shn rotate exit 1` | core | core |
| `shn rotate exit 2` | core | core |
| `shn doctor exit 0` | core | core |
| `shn doctor exit 2` | core | core |
| `shn doctor exit 10` | core | core |
| `shn doctor exit 20` | core | core |
| `shn doctor exit 30` | core | core |
| `shn doctor exit 40` | core | core |

A key directory holds the private signing key (`sign.key`), the private
encryption key (`enc.key`) and the public manifest (`manifest.json`), in the
directory a command's `--out` names (`doctor`'s `--keys`, else its `--out`).
`keygen` writes all three. Both forms of `register` write all three when the
directory holds no `sign.key`, and otherwise read the two keys; `register --accounts`
writes all three again once the Accounts service accepts the registration.
`rotate` reads all three and replaces them. `doctor` reads the two keys.

| Item | Class | Owner |
|---|---|---|
| `sign.key` | core | core |
| `enc.key` | core | core |
| `manifest.json` | core | core |
