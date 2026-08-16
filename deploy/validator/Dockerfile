# FR-G3 config-only validator sidecar: hapiproject/hapi + all eight FHIR IGs baked
# OFFLINE (downloaded at build time, loaded at runtime from file://, no registry
# egress at boot). Same pinned HAPI engine our conformance gates use, so what we
# test is what partners deploy. Single-tenant $validate
# only — NOT a SoR (no URL_BASED partitioning).
#
# ARG SHN_IG_LINE selects which contract line's IG set is baked: "2.0" (default —
# CMS-0057-F-aligned, unchanged from before the contract-version matrix), "2.1",
# or "2.2". Each line is its own pair of named build
# stages (igs-<line> downloads that line's packages; hapi-<line> bakes + configures
# them) so every line's package versions are literal, manifest-verified text —
# never a shell-substituted version string (a mislabeled hapi.fhir.implementation-
# guides.*.version would let the container silently vouch for the WRONG package,
# exactly the failure mode a live two-version IG experiment documented).
# `docker build --build-arg SHN_IG_LINE=2.2 ...` selects
# the 2.2 image; the final `FROM hapi-${SHN_IG_LINE}` picks the matching stage.
ARG SHN_IG_LINE=2.0

# Stage: distroless-safe healthcheck binary (HAPI base has no shell/curl).
FROM golang:1.23-alpine AS hc
WORKDIR /src
COPY go.mod healthcheck.go healthcheck_test.go ./
RUN go test ./... && CGO_ENABLED=0 go build -o /healthcheck healthcheck.go

# Stage: download the 2.0-line IG packages (network at BUILD time only).
FROM alpine:3 AS igs-2.0
RUN apk add --no-cache curl
WORKDIR /packages
RUN curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.core-6.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.core/6.1.0 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-crd-2.0.1.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-crd/2.0.1 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-dtr-2.0.1.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-dtr/2.0.1 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-pas-2.0.1.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-pas/2.0.1 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-pdex-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-pdex/2.1.0 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.uv.sdc-3.0.0.tgz https://packages.simplifier.net/hl7.fhir.uv.sdc/3.0.0 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-cdex-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-cdex/2.1.0 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-hrex-1.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-hrex/1.1.0
# shn.fhir.carry is SHN-authored and not published to a public registry, so it
# cannot be curled like the packages above. It is a COMMITTED build input in
# this context (shnig/, see that directory's README.md): pinned to 1.0.0,
# byte-reproducible from source, and drift-checked against a fresh build.
# Committed rather than staged at build time so this context — which the
# published deploy/bundle/ install unit builds directly — is self-sufficient.
COPY shnig/shn.fhir.carry-1.0.0.tgz /packages/shn.fhir.carry-1.0.0.tgz

# Stage: download the 2.1-line IG packages (network at BUILD time only).
FROM alpine:3 AS igs-2.1
RUN apk add --no-cache curl
WORKDIR /packages
RUN curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.core-7.0.0.tgz https://packages.simplifier.net/hl7.fhir.us.core/7.0.0 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-crd-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-crd/2.1.0 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-dtr-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-dtr/2.1.0 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-pas-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-pas/2.1.0 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-pdex-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-pdex/2.1.0 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.uv.sdc-3.0.0.tgz https://packages.simplifier.net/hl7.fhir.uv.sdc/3.0.0 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-cdex-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-cdex/2.1.0 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-hrex-1.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-hrex/1.1.0
COPY shnig/shn.fhir.carry-1.0.0.tgz /packages/shn.fhir.carry-1.0.0.tgz

# Stage: download the 2.2-line IG packages (network at BUILD time only).
FROM alpine:3 AS igs-2.2
RUN apk add --no-cache curl
WORKDIR /packages
RUN curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.core-7.0.0.tgz https://packages.simplifier.net/hl7.fhir.us.core/7.0.0 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-crd-2.2.1.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-crd/2.2.1 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-dtr-2.2.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-dtr/2.2.0 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-pas-2.2.1.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-pas/2.2.1 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-pdex-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-pdex/2.1.0 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.uv.sdc-4.0.0.tgz https://packages.simplifier.net/hl7.fhir.uv.sdc/4.0.0 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-cdex-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-cdex/2.1.0 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-hrex-1.2.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-hrex/1.2.0 \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.uv.extensions.r4-5.3.0-ballot-tc1.tgz https://packages.simplifier.net/hl7.fhir.uv.extensions.r4/5.3.0-ballot-tc1
COPY shnig/shn.fhir.carry-1.0.0.tgz /packages/shn.fhir.carry-1.0.0.tgz

# Stage: 2.0-line HAPI validator with its IGs baked + loaded from file:// (offline at runtime).
FROM hapiproject/hapi@sha256:1be4d7ffe7a35a9fb46151851e5a20b25c5016f16c8ef8b59b0c807ad06a40c1 AS hapi-2.0
COPY --from=hc /healthcheck /healthcheck
COPY --from=igs-2.0 /packages /packages
ENV hapi.fhir.implementationguides.uscore.packageUrl=file:///packages/hl7.fhir.us.core-6.1.0.tgz \
    hapi.fhir.implementationguides.uscore.name=hl7.fhir.us.core \
    hapi.fhir.implementationguides.uscore.version=6.1.0 \
    hapi.fhir.implementationguides.crd.packageUrl=file:///packages/hl7.fhir.us.davinci-crd-2.0.1.tgz \
    hapi.fhir.implementationguides.crd.name=hl7.fhir.us.davinci-crd \
    hapi.fhir.implementationguides.crd.version=2.0.1 \
    hapi.fhir.implementationguides.dtr.packageUrl=file:///packages/hl7.fhir.us.davinci-dtr-2.0.1.tgz \
    hapi.fhir.implementationguides.dtr.name=hl7.fhir.us.davinci-dtr \
    hapi.fhir.implementationguides.dtr.version=2.0.1 \
    hapi.fhir.implementationguides.pas.packageUrl=file:///packages/hl7.fhir.us.davinci-pas-2.0.1.tgz \
    hapi.fhir.implementationguides.pas.name=hl7.fhir.us.davinci-pas \
    hapi.fhir.implementationguides.pas.version=2.0.1 \
    hapi.fhir.implementationguides.pdex.packageUrl=file:///packages/hl7.fhir.us.davinci-pdex-2.1.0.tgz \
    hapi.fhir.implementationguides.pdex.name=hl7.fhir.us.davinci-pdex \
    hapi.fhir.implementationguides.pdex.version=2.1.0 \
    hapi.fhir.implementationguides.sdc.packageUrl=file:///packages/hl7.fhir.uv.sdc-3.0.0.tgz \
    hapi.fhir.implementationguides.sdc.name=hl7.fhir.uv.sdc \
    hapi.fhir.implementationguides.sdc.version=3.0.0 \
    hapi.fhir.implementationguides.cdex.packageUrl=file:///packages/hl7.fhir.us.davinci-cdex-2.1.0.tgz \
    hapi.fhir.implementationguides.cdex.name=hl7.fhir.us.davinci-cdex \
    hapi.fhir.implementationguides.cdex.version=2.1.0 \
    hapi.fhir.implementationguides.hrex.packageUrl=file:///packages/hl7.fhir.us.davinci-hrex-1.1.0.tgz \
    hapi.fhir.implementationguides.hrex.name=hl7.fhir.us.davinci-hrex \
    hapi.fhir.implementationguides.hrex.version=1.1.0 \
    hapi.fhir.implementationguides.shnig.packageUrl=file:///packages/shn.fhir.carry-1.0.0.tgz \
    hapi.fhir.implementationguides.shnig.name=shn.fhir.carry \
    hapi.fhir.implementationguides.shnig.version=1.0.0

# Stage: 2.1-line HAPI validator with its IGs baked + loaded from file:// (offline at runtime).
FROM hapiproject/hapi@sha256:1be4d7ffe7a35a9fb46151851e5a20b25c5016f16c8ef8b59b0c807ad06a40c1 AS hapi-2.1
COPY --from=hc /healthcheck /healthcheck
COPY --from=igs-2.1 /packages /packages
ENV hapi.fhir.implementationguides.uscore.packageUrl=file:///packages/hl7.fhir.us.core-7.0.0.tgz \
    hapi.fhir.implementationguides.uscore.name=hl7.fhir.us.core \
    hapi.fhir.implementationguides.uscore.version=7.0.0 \
    hapi.fhir.implementationguides.crd.packageUrl=file:///packages/hl7.fhir.us.davinci-crd-2.1.0.tgz \
    hapi.fhir.implementationguides.crd.name=hl7.fhir.us.davinci-crd \
    hapi.fhir.implementationguides.crd.version=2.1.0 \
    hapi.fhir.implementationguides.dtr.packageUrl=file:///packages/hl7.fhir.us.davinci-dtr-2.1.0.tgz \
    hapi.fhir.implementationguides.dtr.name=hl7.fhir.us.davinci-dtr \
    hapi.fhir.implementationguides.dtr.version=2.1.0 \
    hapi.fhir.implementationguides.pas.packageUrl=file:///packages/hl7.fhir.us.davinci-pas-2.1.0.tgz \
    hapi.fhir.implementationguides.pas.name=hl7.fhir.us.davinci-pas \
    hapi.fhir.implementationguides.pas.version=2.1.0 \
    hapi.fhir.implementationguides.pdex.packageUrl=file:///packages/hl7.fhir.us.davinci-pdex-2.1.0.tgz \
    hapi.fhir.implementationguides.pdex.name=hl7.fhir.us.davinci-pdex \
    hapi.fhir.implementationguides.pdex.version=2.1.0 \
    hapi.fhir.implementationguides.sdc.packageUrl=file:///packages/hl7.fhir.uv.sdc-3.0.0.tgz \
    hapi.fhir.implementationguides.sdc.name=hl7.fhir.uv.sdc \
    hapi.fhir.implementationguides.sdc.version=3.0.0 \
    hapi.fhir.implementationguides.cdex.packageUrl=file:///packages/hl7.fhir.us.davinci-cdex-2.1.0.tgz \
    hapi.fhir.implementationguides.cdex.name=hl7.fhir.us.davinci-cdex \
    hapi.fhir.implementationguides.cdex.version=2.1.0 \
    hapi.fhir.implementationguides.hrex.packageUrl=file:///packages/hl7.fhir.us.davinci-hrex-1.1.0.tgz \
    hapi.fhir.implementationguides.hrex.name=hl7.fhir.us.davinci-hrex \
    hapi.fhir.implementationguides.hrex.version=1.1.0 \
    hapi.fhir.implementationguides.shnig.packageUrl=file:///packages/shn.fhir.carry-1.0.0.tgz \
    hapi.fhir.implementationguides.shnig.name=shn.fhir.carry \
    hapi.fhir.implementationguides.shnig.version=1.0.0

# Stage: 2.2-line HAPI validator with its IGs baked + loaded from file:// (offline at runtime).
FROM hapiproject/hapi@sha256:1be4d7ffe7a35a9fb46151851e5a20b25c5016f16c8ef8b59b0c807ad06a40c1 AS hapi-2.2
COPY --from=hc /healthcheck /healthcheck
COPY --from=igs-2.2 /packages /packages
ENV hapi.fhir.implementationguides.uscore.packageUrl=file:///packages/hl7.fhir.us.core-7.0.0.tgz \
    hapi.fhir.implementationguides.uscore.name=hl7.fhir.us.core \
    hapi.fhir.implementationguides.uscore.version=7.0.0 \
    hapi.fhir.implementationguides.crd.packageUrl=file:///packages/hl7.fhir.us.davinci-crd-2.2.1.tgz \
    hapi.fhir.implementationguides.crd.name=hl7.fhir.us.davinci-crd \
    hapi.fhir.implementationguides.crd.version=2.2.1 \
    hapi.fhir.implementationguides.dtr.packageUrl=file:///packages/hl7.fhir.us.davinci-dtr-2.2.0.tgz \
    hapi.fhir.implementationguides.dtr.name=hl7.fhir.us.davinci-dtr \
    hapi.fhir.implementationguides.dtr.version=2.2.0 \
    hapi.fhir.implementationguides.pas.packageUrl=file:///packages/hl7.fhir.us.davinci-pas-2.2.1.tgz \
    hapi.fhir.implementationguides.pas.name=hl7.fhir.us.davinci-pas \
    hapi.fhir.implementationguides.pas.version=2.2.1 \
    hapi.fhir.implementationguides.pdex.packageUrl=file:///packages/hl7.fhir.us.davinci-pdex-2.1.0.tgz \
    hapi.fhir.implementationguides.pdex.name=hl7.fhir.us.davinci-pdex \
    hapi.fhir.implementationguides.pdex.version=2.1.0 \
    hapi.fhir.implementationguides.sdc.packageUrl=file:///packages/hl7.fhir.uv.sdc-4.0.0.tgz \
    hapi.fhir.implementationguides.sdc.name=hl7.fhir.uv.sdc \
    hapi.fhir.implementationguides.sdc.version=4.0.0 \
    hapi.fhir.implementationguides.cdex.packageUrl=file:///packages/hl7.fhir.us.davinci-cdex-2.1.0.tgz \
    hapi.fhir.implementationguides.cdex.name=hl7.fhir.us.davinci-cdex \
    hapi.fhir.implementationguides.cdex.version=2.1.0 \
    hapi.fhir.implementationguides.hrex.packageUrl=file:///packages/hl7.fhir.us.davinci-hrex-1.2.0.tgz \
    hapi.fhir.implementationguides.hrex.name=hl7.fhir.us.davinci-hrex \
    hapi.fhir.implementationguides.hrex.version=1.2.0 \
    hapi.fhir.implementationguides.extensions.packageUrl=file:///packages/hl7.fhir.uv.extensions.r4-5.3.0-ballot-tc1.tgz \
    hapi.fhir.implementationguides.extensions.name=hl7.fhir.uv.extensions.r4 \
    hapi.fhir.implementationguides.extensions.version=5.3.0-ballot-tc1 \
    hapi.fhir.implementationguides.shnig.packageUrl=file:///packages/shn.fhir.carry-1.0.0.tgz \
    hapi.fhir.implementationguides.shnig.name=shn.fhir.carry \
    hapi.fhir.implementationguides.shnig.version=1.0.0

# Final stage selection: ARG SHN_IG_LINE (declared above, before the first FROM)
# picks which of the three fully-baked hapi-<line> stages above becomes the image.
FROM hapi-${SHN_IG_LINE}
