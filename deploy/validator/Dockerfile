# FR-G3 config-only validator sidecar: hapiproject/hapi + all six FHIR IGs baked
# OFFLINE (downloaded at build time, loaded at runtime from file://, no registry
# egress at boot). Same pinned HAPI engine our conformance gates use, so what we
# test is what partners deploy. Single-tenant $validate
# only — NOT a SoR (no URL_BASED partitioning).

# Stage 1: distroless-safe healthcheck binary (HAPI base has no shell/curl).
FROM golang:1.23-alpine AS hc
WORKDIR /src
COPY go.mod healthcheck.go healthcheck_test.go ./
RUN go test ./... && CGO_ENABLED=0 go build -o /healthcheck healthcheck.go

# Stage 2: download the IG packages (network at BUILD time only).
FROM alpine:3 AS igs
RUN apk add --no-cache curl
WORKDIR /packages
RUN curl -fLSso hl7.fhir.us.core-6.1.0.tgz         https://packages.simplifier.net/hl7.fhir.us.core/6.1.0 \
 && curl -fLSso hl7.fhir.us.davinci-crd-2.0.1.tgz  https://packages.simplifier.net/hl7.fhir.us.davinci-crd/2.0.1 \
 && curl -fLSso hl7.fhir.us.davinci-dtr-2.0.1.tgz  https://packages.simplifier.net/hl7.fhir.us.davinci-dtr/2.0.1 \
 && curl -fLSso hl7.fhir.us.davinci-pas-2.0.1.tgz  https://packages.simplifier.net/hl7.fhir.us.davinci-pas/2.0.1 \
 && curl -fLSso hl7.fhir.us.davinci-pdex-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-pdex/2.1.0 \
 && curl -fLSso hl7.fhir.uv.sdc-3.0.0.tgz          https://packages.simplifier.net/hl7.fhir.uv.sdc/3.0.0

# Stage 3: HAPI validator with the IGs baked + loaded from file:// (offline at runtime).
FROM hapiproject/hapi@sha256:1be4d7ffe7a35a9fb46151851e5a20b25c5016f16c8ef8b59b0c807ad06a40c1
COPY --from=hc /healthcheck /healthcheck
COPY --from=igs /packages /packages
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
    hapi.fhir.implementationguides.sdc.version=3.0.0
