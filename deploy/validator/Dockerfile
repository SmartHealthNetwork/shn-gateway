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

# Stage: distroless-safe healthcheck binary (HAPI base has no shell/curl). Readiness
# is "can validate": PID 1 owns one sequential warm-up worker and withholds public
# forwarding until it succeeds on Java's private loopback endpoint. Ordinary probes
# only observe public metadata and the marker. SHN_IG_LINE selects embedded fixtures.
FROM hapiproject/hapi@sha256:1be4d7ffe7a35a9fb46151851e5a20b25c5016f16c8ef8b59b0c807ad06a40c1 AS backport-engine
FROM eclipse-temurin:21.0.11_10-jdk-jammy@sha256:dbfd085220ae632a0830166e443747d1ee89e9038d92e3b48c3e5e9d8292b9a7 AS backport
ARG TARGETPLATFORM
COPY --from=backport-engine /app/main.war /input/main.war
COPY backport /src
RUN --network=none case "$TARGETPLATFORM" in \
      linux/arm64) source_child=sha256:ac871f44ee311bdd4d3533f7ea9e81b3a744ca709726b74d5a97d29663a7916c; compiler_child=sha256:4cc4a72887c92db0128b0725443a80291171759f9731300c12a63403504f76c3 ;; \
      linux/amd64) source_child=sha256:f4aa83b7102a52f19a42c4c27c5f63fa3d1ded4f393c7083e53a39dc37b2a4c5; compiler_child=sha256:ea0e59a081c886d919b177b7006ac02fbdc64e8ab7cae724e2afd62de42d6892 ;; \
      *) exit 2 ;; \
    esac && /src/build.sh /input/main.war /out "$TARGETPLATFORM" "$source_child" "$compiler_child"

FROM golang:1.23-alpine AS hc
WORKDIR /src
COPY go.mod *.go ./
COPY verify-process.sh ./
COPY testdata ./testdata
RUN go test ./... && CGO_ENABLED=0 go build -o /healthcheck .

# Stage: download the 2.0-line IG packages (network at BUILD time only).
FROM alpine:3 AS igs-2.0
RUN apk add --no-cache curl
WORKDIR /packages
RUN curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.core-6.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.core/6.1.0 \
 && echo "ec096f29bc4ce8f04117dfc4ef82e7289c67a7e2188892e86288c45389d158d9  hl7.fhir.us.core-6.1.0.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-crd-2.0.1.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-crd/2.0.1 \
 && echo "977ac1841346de0d9b50896dddd4834175ab41c6e25a58ca12e266f95a1c17fe  hl7.fhir.us.davinci-crd-2.0.1.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-dtr-2.0.1.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-dtr/2.0.1 \
 && echo "a2840c31da3277d7b78c74baf65bcbeee97ddfe4f956810bffd79d9cfe5c64f0  hl7.fhir.us.davinci-dtr-2.0.1.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-pas-2.0.1.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-pas/2.0.1 \
 && echo "1ba4159f2a1ea000dfb1685601bfc902c603a1c59ce2127118ee0a7aec694218  hl7.fhir.us.davinci-pas-2.0.1.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-pdex-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-pdex/2.1.0 \
 && echo "da9e8b3a1e702e911a4473545e22e753d3219d2374ddd7889994ae0e38929571  hl7.fhir.us.davinci-pdex-2.1.0.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.uv.sdc-3.0.0.tgz https://packages.simplifier.net/hl7.fhir.uv.sdc/3.0.0 \
 && echo "be5c3f0a7f82e072b40739e05fb68f3e58e44c9bd040cd901b1b55c62f23dca2  hl7.fhir.uv.sdc-3.0.0.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-cdex-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-cdex/2.1.0 \
 && echo "7c950274d8128934868254e1a620dd44d63d9f62bed36edaa70018b70cd1102d  hl7.fhir.us.davinci-cdex-2.1.0.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-hrex-1.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-hrex/1.1.0 \
 && echo "a122c031bc7d285a7aff18e18462728e499043e47f3675f43d4b359884287359  hl7.fhir.us.davinci-hrex-1.1.0.tgz" | sha256sum -c
# shn.fhir.carry is SHN-authored and not published to a public registry, so it
# cannot be curled like the packages above. It is a COMMITTED build input in
# this context (shnig/, see that directory's README.md): pinned to 1.0.0,
# byte-reproducible from source, and drift-checked against a fresh build.
# Committed rather than staged at build time so this context — which the
# published deploy/bundle/ install unit builds directly — is self-sufficient.
COPY shnig/shn.fhir.carry-1.0.0.tgz /packages/shn.fhir.carry-1.0.0.tgz
COPY support/shn.fhir.validation-support-1.4.0.tgz /packages/shn.fhir.validation-support-1.4.0.tgz

# Stage: download the 2.1-line IG packages (network at BUILD time only).
FROM alpine:3 AS igs-2.1
RUN apk add --no-cache curl
WORKDIR /packages
RUN curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.core-7.0.0.tgz https://packages.simplifier.net/hl7.fhir.us.core/7.0.0 \
 && echo "f47243e558b0a91104a2f80d4269d974d89862227e62ca2068f5bc4cba625737  hl7.fhir.us.core-7.0.0.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-crd-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-crd/2.1.0 \
 && echo "7f77431ec5b2bf9f0569a790c170f68104be208f8167b1368deb0d58a5ab82c4  hl7.fhir.us.davinci-crd-2.1.0.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-dtr-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-dtr/2.1.0 \
 && echo "4e6e1b3c26b41d75433cb3c16345272b59ec40d00667245d07610b74779df640  hl7.fhir.us.davinci-dtr-2.1.0.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-pas-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-pas/2.1.0 \
 && echo "a0daef9c5acf6954c15ebd713477ce49962e56c0c2ac4362d1381aee8f7892df  hl7.fhir.us.davinci-pas-2.1.0.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-pdex-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-pdex/2.1.0 \
 && echo "da9e8b3a1e702e911a4473545e22e753d3219d2374ddd7889994ae0e38929571  hl7.fhir.us.davinci-pdex-2.1.0.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.uv.sdc-3.0.0.tgz https://packages.simplifier.net/hl7.fhir.uv.sdc/3.0.0 \
 && echo "be5c3f0a7f82e072b40739e05fb68f3e58e44c9bd040cd901b1b55c62f23dca2  hl7.fhir.uv.sdc-3.0.0.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-cdex-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-cdex/2.1.0 \
 && echo "7c950274d8128934868254e1a620dd44d63d9f62bed36edaa70018b70cd1102d  hl7.fhir.us.davinci-cdex-2.1.0.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-hrex-1.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-hrex/1.1.0 \
 && echo "a122c031bc7d285a7aff18e18462728e499043e47f3675f43d4b359884287359  hl7.fhir.us.davinci-hrex-1.1.0.tgz" | sha256sum -c
COPY shnig/shn.fhir.carry-1.0.0.tgz /packages/shn.fhir.carry-1.0.0.tgz
COPY support/shn.fhir.validation-support-1.4.0.tgz /packages/shn.fhir.validation-support-1.4.0.tgz

# Stage: download the 2.2-line IG packages (network at BUILD time only).
FROM alpine:3 AS igs-2.2
RUN apk add --no-cache curl
WORKDIR /packages
RUN curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.core-7.0.0.tgz https://packages.simplifier.net/hl7.fhir.us.core/7.0.0 \
 && echo "f47243e558b0a91104a2f80d4269d974d89862227e62ca2068f5bc4cba625737  hl7.fhir.us.core-7.0.0.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-crd-2.2.1.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-crd/2.2.1 \
 && echo "9ce47b6a53f7c138a5c5952ddea2fbdaf282c346aae1d3a3947664e3e5e16525  hl7.fhir.us.davinci-crd-2.2.1.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-dtr-2.2.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-dtr/2.2.0 \
 && echo "027894027a86e3a250252ac61476f82ca509a2ed04dbd4bad0816d2e24f204ea  hl7.fhir.us.davinci-dtr-2.2.0.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-pas-2.2.1.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-pas/2.2.1 \
 && echo "da2aac8e480ce66fe27812af18e3c9bc190b08a33d5d7e1c850caf9097fb210e  hl7.fhir.us.davinci-pas-2.2.1.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-pdex-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-pdex/2.1.0 \
 && echo "da9e8b3a1e702e911a4473545e22e753d3219d2374ddd7889994ae0e38929571  hl7.fhir.us.davinci-pdex-2.1.0.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.uv.sdc-4.0.0.tgz https://packages.simplifier.net/hl7.fhir.uv.sdc/4.0.0 \
 && echo "d785be8474c7ec7988e32e326430d9ca4aeb2cac4a1f195022e4d6f7dc5c5291  hl7.fhir.uv.sdc-4.0.0.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-cdex-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-cdex/2.1.0 \
 && echo "7c950274d8128934868254e1a620dd44d63d9f62bed36edaa70018b70cd1102d  hl7.fhir.us.davinci-cdex-2.1.0.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-hrex-1.2.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-hrex/1.2.0 \
 && echo "f28749c04bed6c791aa8aa594dbaffcdbabd4605d650860389dc2c2c61173c44  hl7.fhir.us.davinci-hrex-1.2.0.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.uv.extensions.r4-5.3.0-ballot-tc1.tgz https://packages.simplifier.net/hl7.fhir.uv.extensions.r4/5.3.0-ballot-tc1 \
 && echo "5f5d1e88052d615453e6b7a1eb4f885df36590f14b52974e5860485d16819bd9  hl7.fhir.uv.extensions.r4-5.3.0-ballot-tc1.tgz" | sha256sum -c
COPY shnig/shn.fhir.carry-1.0.0.tgz /packages/shn.fhir.carry-1.0.0.tgz
COPY support/shn.fhir.validation-support-1.4.0.tgz /packages/shn.fhir.validation-support-1.4.0.tgz

# Stage: 2.0-line HAPI validator with its IGs baked + loaded from file:// (offline at runtime).
FROM hapiproject/hapi@sha256:1be4d7ffe7a35a9fb46151851e5a20b25c5016f16c8ef8b59b0c807ad06a40c1 AS hapi-2.0
COPY --from=backport /out/main.war /app/main.war
COPY --from=backport /out/provenance.json /app/backport-provenance.json
LABEL org.shn.validator.backport.class="51c76b59364b8927a40efb29232945289974ff540b53b623adc9031886bb47f5"
COPY --from=hc /healthcheck /healthcheck
# Preserve the pinned base image's literal Java argv, inherited env/user/workdir.
ENTRYPOINT ["/healthcheck", "supervise", "java", "--class-path", "/app/main.war", "-Dloader.path=main.war!/WEB-INF/classes/,main.war!/WEB-INF/,/app/extra-classes", "org.springframework.boot.loader.PropertiesLauncher"]
COPY --from=igs-2.0 /packages /packages
ENV SHN_IG_LINE=2.0 \
    hapi.fhir.implementationguides.uscore.packageUrl=file:///packages/hl7.fhir.us.core-6.1.0.tgz \
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
    hapi.fhir.implementationguides.shnig.version=1.0.0 \
    hapi.fhir.implementationguides.validationsupport.packageUrl=file:///packages/shn.fhir.validation-support-1.4.0.tgz \
    hapi.fhir.implementationguides.validationsupport.name=shn.fhir.validation-support \
    hapi.fhir.implementationguides.validationsupport.version=1.4.0

# Stage: 2.1-line HAPI validator with its IGs baked + loaded from file:// (offline at runtime).
FROM hapiproject/hapi@sha256:1be4d7ffe7a35a9fb46151851e5a20b25c5016f16c8ef8b59b0c807ad06a40c1 AS hapi-2.1
COPY --from=backport /out/main.war /app/main.war
COPY --from=backport /out/provenance.json /app/backport-provenance.json
LABEL org.shn.validator.backport.class="51c76b59364b8927a40efb29232945289974ff540b53b623adc9031886bb47f5"
COPY --from=hc /healthcheck /healthcheck
# Preserve the pinned base image's literal Java argv, inherited env/user/workdir.
ENTRYPOINT ["/healthcheck", "supervise", "java", "--class-path", "/app/main.war", "-Dloader.path=main.war!/WEB-INF/classes/,main.war!/WEB-INF/,/app/extra-classes", "org.springframework.boot.loader.PropertiesLauncher"]
COPY --from=igs-2.1 /packages /packages
ENV SHN_IG_LINE=2.1 \
    hapi.fhir.implementationguides.uscore.packageUrl=file:///packages/hl7.fhir.us.core-7.0.0.tgz \
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
    hapi.fhir.implementationguides.shnig.version=1.0.0 \
    hapi.fhir.implementationguides.validationsupport.packageUrl=file:///packages/shn.fhir.validation-support-1.4.0.tgz \
    hapi.fhir.implementationguides.validationsupport.name=shn.fhir.validation-support \
    hapi.fhir.implementationguides.validationsupport.version=1.4.0

# Stage: 2.2-line HAPI validator with its IGs baked + loaded from file:// (offline at runtime).
FROM hapiproject/hapi@sha256:1be4d7ffe7a35a9fb46151851e5a20b25c5016f16c8ef8b59b0c807ad06a40c1 AS hapi-2.2
COPY --from=backport /out/main.war /app/main.war
COPY --from=backport /out/provenance.json /app/backport-provenance.json
LABEL org.shn.validator.backport.class="51c76b59364b8927a40efb29232945289974ff540b53b623adc9031886bb47f5"
COPY --from=hc /healthcheck /healthcheck
# Preserve the pinned base image's literal Java argv, inherited env/user/workdir.
ENTRYPOINT ["/healthcheck", "supervise", "java", "--class-path", "/app/main.war", "-Dloader.path=main.war!/WEB-INF/classes/,main.war!/WEB-INF/,/app/extra-classes", "org.springframework.boot.loader.PropertiesLauncher"]
COPY --from=igs-2.2 /packages /packages
ENV SHN_IG_LINE=2.2 \
    hapi.fhir.implementationguides.uscore.packageUrl=file:///packages/hl7.fhir.us.core-7.0.0.tgz \
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
    hapi.fhir.implementationguides.shnig.version=1.0.0 \
    hapi.fhir.implementationguides.validationsupport.packageUrl=file:///packages/shn.fhir.validation-support-1.4.0.tgz \
    hapi.fhir.implementationguides.validationsupport.name=shn.fhir.validation-support \
    hapi.fhir.implementationguides.validationsupport.version=1.4.0

# Final stage selection: ARG SHN_IG_LINE (declared above, before the first FROM)
# picks which of the three fully-baked hapi-<line> stages above becomes the image.
FROM hapi-${SHN_IG_LINE}
