# Offline-bake the 4 IGs (US Core + CDex + HRex + PAS) the separated-stack data-HAPI validates
# against, so a Simplifier outage during a separated-stack boot can't fail IG load
# (the UC-05 422 hardening). Mirrors gateway/deploy/validator/Dockerfile's igs stage. The
# data-HAPI's multitenancy + operated-CQL (cr.enabled) config lives in compose.multiprocess.yml;
# this Dockerfile only bakes the tgz and points file:/// at them.
FROM alpine:3 AS igs
RUN apk add --no-cache curl
WORKDIR /packages
RUN curl -fLSso hl7.fhir.us.core-6.1.0.tgz         https://packages.simplifier.net/hl7.fhir.us.core/6.1.0 \
 && curl -fLSso hl7.fhir.us.davinci-cdex-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-cdex/2.1.0 \
 && curl -fLSso hl7.fhir.us.davinci-hrex-1.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-hrex/1.1.0 \
 && curl -fLSso hl7.fhir.us.davinci-pas-2.0.1.tgz  https://packages.simplifier.net/hl7.fhir.us.davinci-pas/2.0.1

# Same digest the repo pins everywhere (the engine the conformance gates validate against).
FROM hapiproject/hapi@sha256:1be4d7ffe7a35a9fb46151851e5a20b25c5016f16c8ef8b59b0c807ad06a40c1
COPY --from=igs /packages /packages
