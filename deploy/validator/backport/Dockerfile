# Build-only source packaging. Runtime consumers copy the WAR and provenance.
# Built from the directory above this one (cache.py passes -f), so its backport
# stage is the validator Dockerfile's, instruction for instruction: a host build
# and an image build on one Docker daemon compile the WAR once.
FROM hapiproject/hapi@sha256:0ff86aac4c7bd13a1ee226444a70263bb46ed1c28ef543c30a44f865b80ca04e AS backport-engine
FROM eclipse-temurin:21.0.11_10-jdk-jammy@sha256:dbfd085220ae632a0830166e443747d1ee89e9038d92e3b48c3e5e9d8292b9a7 AS backport
ARG TARGETPLATFORM
COPY --from=backport-engine /app/main.war /input/main.war
COPY backport /src
RUN --network=none case "$TARGETPLATFORM" in \
      linux/arm64) source_child=sha256:4d59628d42b3556b2dcd274d26054a43ba53697ebdae0fd71e6a5475065ed22d; compiler_child=sha256:4cc4a72887c92db0128b0725443a80291171759f9731300c12a63403504f76c3 ;; \
      linux/amd64) source_child=sha256:88489ad986f38d35ec0e216a0660db42cb55329275efdc5913dfe1d2f2afbe53; compiler_child=sha256:ea0e59a081c886d919b177b7006ac02fbdc64e8ab7cae724e2afd62de42d6892 ;; \
      *) exit 2 ;; \
    esac && /src/build.sh /input/main.war /out "$TARGETPLATFORM" "$source_child" "$compiler_child"
FROM scratch AS artifacts
COPY --from=backport /out/ /
