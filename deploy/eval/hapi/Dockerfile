# Offline-bake the IGs this bundle's data-HAPI validates against (US Core, CDex, HRex, and
# PAS, CRD, DTR and SDC at the 2.0 line, plus SHN's validation-support package), so a Simplifier
# outage during a boot can't fail IG load. The gateway validates what it sends here
# (FHIR_VALIDATE_URL), the QuestionnaireResponse it declares DTR's profile on included; it is the
# same set the platform's own data-HAPI loads, and the validator image's (../validator/Dockerfile)
# 2.0 lane. The data-HAPI's IG list, multitenancy and operated-CQL (cr.enabled) settings live in
# compose.eval.yml; this Dockerfile only bakes the tgz files the list points at (file:///).
FROM hapiproject/hapi@sha256:0ff86aac4c7bd13a1ee226444a70263bb46ed1c28ef543c30a44f865b80ca04e AS backport-engine
FROM eclipse-temurin:21.0.11_10-jdk-jammy@sha256:dbfd085220ae632a0830166e443747d1ee89e9038d92e3b48c3e5e9d8292b9a7 AS backport
ARG TARGETPLATFORM
COPY --from=backport-engine /app/main.war /input/main.war
COPY validator/backport /src
RUN --network=none case "$TARGETPLATFORM" in \
      linux/arm64) source_child=sha256:4d59628d42b3556b2dcd274d26054a43ba53697ebdae0fd71e6a5475065ed22d; compiler_child=sha256:4cc4a72887c92db0128b0725443a80291171759f9731300c12a63403504f76c3 ;; \
      linux/amd64) source_child=sha256:88489ad986f38d35ec0e216a0660db42cb55329275efdc5913dfe1d2f2afbe53; compiler_child=sha256:ea0e59a081c886d919b177b7006ac02fbdc64e8ab7cae724e2afd62de42d6892 ;; \
      *) exit 2 ;; \
    esac && /src/build.sh /input/main.war /out "$TARGETPLATFORM" "$source_child" "$compiler_child"

# The IG packages a build may pass as the igpackages build context
# (`--build-context igpackages=<dir>`, compose `additional_contexts`): a
# directory of <name>-<version>.tgz archives. Empty by default, so every
# package below is downloaded from the registry. Each package, from either
# source, is checked against its pinned sha256 before the image uses it, and
# IG_PACKAGES_OFFLINE=1 refuses the registry: a package the context does not
# hold then fails the build.
FROM scratch AS igpackages

FROM alpine:3 AS igs
ARG IG_PACKAGES_OFFLINE=
RUN [ "$IG_PACKAGES_OFFLINE" = 1 ] || apk add --no-cache curl
COPY --from=igpackages / /igcache/
WORKDIR /packages
RUN igfetch() { if [ -f "/igcache/$1" ]; then cp "/igcache/$1" "$1"; elif [ "$IG_PACKAGES_OFFLINE" = 1 ]; then echo "igfetch: $1 is not in the igpackages build context, and IG_PACKAGES_OFFLINE=1 refuses the registry" >&2; return 1; else curl --retry 5 --retry-delay 5 --retry-all-errors -fLSso "$1" "$2"; fi; } \
 && igfetch hl7.fhir.us.core-6.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.core/6.1.0 \
 && echo "ec096f29bc4ce8f04117dfc4ef82e7289c67a7e2188892e86288c45389d158d9  hl7.fhir.us.core-6.1.0.tgz" | sha256sum -c \
 && igfetch hl7.fhir.us.davinci-cdex-2.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-cdex/2.1.0 \
 && echo "7c950274d8128934868254e1a620dd44d63d9f62bed36edaa70018b70cd1102d  hl7.fhir.us.davinci-cdex-2.1.0.tgz" | sha256sum -c \
 && igfetch hl7.fhir.us.davinci-hrex-1.1.0.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-hrex/1.1.0 \
 && echo "a122c031bc7d285a7aff18e18462728e499043e47f3675f43d4b359884287359  hl7.fhir.us.davinci-hrex-1.1.0.tgz" | sha256sum -c \
 && igfetch hl7.fhir.us.davinci-pas-2.0.1.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-pas/2.0.1 \
 && echo "1ba4159f2a1ea000dfb1685601bfc902c603a1c59ce2127118ee0a7aec694218  hl7.fhir.us.davinci-pas-2.0.1.tgz" | sha256sum -c \
 && igfetch hl7.fhir.us.davinci-crd-2.0.1.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-crd/2.0.1 \
 && echo "977ac1841346de0d9b50896dddd4834175ab41c6e25a58ca12e266f95a1c17fe  hl7.fhir.us.davinci-crd-2.0.1.tgz" | sha256sum -c \
 && igfetch hl7.fhir.us.davinci-dtr-2.0.1.tgz https://packages.simplifier.net/hl7.fhir.us.davinci-dtr/2.0.1 \
 && echo "a2840c31da3277d7b78c74baf65bcbeee97ddfe4f956810bffd79d9cfe5c64f0  hl7.fhir.us.davinci-dtr-2.0.1.tgz" | sha256sum -c \
 && igfetch hl7.fhir.uv.sdc-3.0.0.tgz https://packages.simplifier.net/hl7.fhir.uv.sdc/3.0.0 \
 && echo "be5c3f0a7f82e072b40739e05fb68f3e58e44c9bd040cd901b1b55c62f23dca2  hl7.fhir.uv.sdc-3.0.0.tgz" | sha256sum -c

# The runtime base's JVM trust store without the four eMudhra emSign roots
# (CVE-2026-114820): the JRE reads lib/security/cacerts through links to
# /etc/ssl/certs/java/cacerts, and the runtime stage copies this edited store
# over it. The store comes from the same pinned base as the runtime stage.
FROM eclipse-temurin:21.0.11_10-jdk-jammy@sha256:dbfd085220ae632a0830166e443747d1ee89e9038d92e3b48c3e5e9d8292b9a7 AS truststore
COPY --from=gcr.io/distroless/java21-debian13:nonroot@sha256:0a1f5a75661918de9c0813f287f651c3bf2d6dd752eada5f084eb0c1f14ced9e /etc/ssl/certs/java/cacerts /in/cacerts
COPY validator/truststore/drop-emsign-roots.sh /truststore/drop-emsign-roots.sh
RUN --network=none bash /truststore/drop-emsign-roots.sh /in/cacerts /out/cacerts

# The backported WAR on the pinned distroless Java base upstream builds HAPI on
# (same JRE and env), without upstream's unused bundled jars.
FROM gcr.io/distroless/java21-debian13:nonroot@sha256:0a1f5a75661918de9c0813f287f651c3bf2d6dd752eada5f084eb0c1f14ced9e
USER 65532:65532
COPY --from=truststore /out/cacerts /etc/ssl/certs/java/cacerts
WORKDIR /app
# The HAPI starter serves an MCP server by default (/mcp/messages, /sse), a second way into
# the FHIR server that nothing here uses: it stays off.
ENV SPRING_AI_MCP_SERVER_ENABLED=false
ENTRYPOINT ["java", "--class-path", "/app/main.war", "-Dloader.path=main.war!/WEB-INF/classes/,main.war!/WEB-INF/,/app/extra-classes", "org.springframework.boot.loader.PropertiesLauncher"]
COPY --from=backport /out/main.war /app/main.war
COPY --from=backport /out/provenance.json /app/backport-provenance.json
LABEL org.shn.validator.backport.class="51c76b59364b8927a40efb29232945289974ff540b53b623adc9031886bb47f5"
COPY --from=igs /packages /packages
COPY validator/support/shn.fhir.validation-support-1.5.0.tgz /packages/shn.fhir.validation-support-1.5.0.tgz
