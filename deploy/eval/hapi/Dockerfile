# Offline-bake the 4 IGs (US Core + CDex + HRex + PAS) the separated-stack data-HAPI validates
# against, so a Simplifier outage during a separated-stack boot can't fail IG load
# (the UC-05 422 hardening). Mirrors gateway/deploy/validator/Dockerfile's igs stage. The
# data-HAPI's multitenancy + operated-CQL (cr.enabled) config lives in compose.multiprocess.yml;
# this Dockerfile only bakes the tgz and points file:/// at them.
FROM hapiproject/hapi@sha256:1be4d7ffe7a35a9fb46151851e5a20b25c5016f16c8ef8b59b0c807ad06a40c1 AS backport-engine
FROM eclipse-temurin:21.0.11_10-jdk-jammy@sha256:dbfd085220ae632a0830166e443747d1ee89e9038d92e3b48c3e5e9d8292b9a7 AS backport
ARG TARGETPLATFORM
COPY --from=backport-engine /app/main.war /input/main.war
COPY validator/backport /src
RUN --network=none case "$TARGETPLATFORM" in \
      linux/arm64) source_child=sha256:ac871f44ee311bdd4d3533f7ea9e81b3a744ca709726b74d5a97d29663a7916c; compiler_child=sha256:4cc4a72887c92db0128b0725443a80291171759f9731300c12a63403504f76c3 ;; \
      linux/amd64) source_child=sha256:f4aa83b7102a52f19a42c4c27c5f63fa3d1ded4f393c7083e53a39dc37b2a4c5; compiler_child=sha256:ea0e59a081c886d919b177b7006ac02fbdc64e8ab7cae724e2afd62de42d6892 ;; \
      *) exit 2 ;; \
    esac && /src/build.sh /input/main.war /out "$TARGETPLATFORM" "$source_child" "$compiler_child"

FROM alpine:3 AS igs
RUN apk add --no-cache curl
WORKDIR /packages
RUN curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.core-6.1.0.tgz         https://packages.simplifier.net/hl7.fhir.us.core/6.1.0 \
 && echo "ec096f29bc4ce8f04117dfc4ef82e7289c67a7e2188892e86288c45389d158d9  hl7.fhir.us.core-6.1.0.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-cdex-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-cdex/2.1.0 \
 && echo "7c950274d8128934868254e1a620dd44d63d9f62bed36edaa70018b70cd1102d  hl7.fhir.us.davinci-cdex-2.1.0.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-hrex-1.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-hrex/1.1.0 \
 && echo "a122c031bc7d285a7aff18e18462728e499043e47f3675f43d4b359884287359  hl7.fhir.us.davinci-hrex-1.1.0.tgz" | sha256sum -c \
 && curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso hl7.fhir.us.davinci-pas-2.0.1.tgz  https://packages.simplifier.net/hl7.fhir.us.davinci-pas/2.0.1 \
 && echo "1ba4159f2a1ea000dfb1685601bfc902c603a1c59ce2127118ee0a7aec694218  hl7.fhir.us.davinci-pas-2.0.1.tgz" | sha256sum -c

# Same digest the repo pins everywhere (the engine the conformance gates validate against).
FROM hapiproject/hapi@sha256:1be4d7ffe7a35a9fb46151851e5a20b25c5016f16c8ef8b59b0c807ad06a40c1
COPY --from=backport /out/main.war /app/main.war
COPY --from=backport /out/provenance.json /app/backport-provenance.json
LABEL org.shn.validator.backport.class="51c76b59364b8927a40efb29232945289974ff540b53b623adc9031886bb47f5"
COPY --from=igs /packages /packages
