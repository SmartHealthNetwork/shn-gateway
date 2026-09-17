# Build-only source packaging. Runtime consumers copy the WAR and provenance.
FROM hapiproject/hapi@sha256:1be4d7ffe7a35a9fb46151851e5a20b25c5016f16c8ef8b59b0c807ad06a40c1 AS engine
FROM eclipse-temurin:21.0.11_10-jdk-jammy@sha256:dbfd085220ae632a0830166e443747d1ee89e9038d92e3b48c3e5e9d8292b9a7 AS backport
ARG TARGETPLATFORM
COPY --from=engine /app/main.war /input/main.war
COPY . /src
RUN --network=none case "$TARGETPLATFORM" in \
      linux/arm64) source_child=sha256:ac871f44ee311bdd4d3533f7ea9e81b3a744ca709726b74d5a97d29663a7916c; compiler_child=sha256:4cc4a72887c92db0128b0725443a80291171759f9731300c12a63403504f76c3 ;; \
      linux/amd64) source_child=sha256:f4aa83b7102a52f19a42c4c27c5f63fa3d1ded4f393c7083e53a39dc37b2a4c5; compiler_child=sha256:ea0e59a081c886d919b177b7006ac02fbdc64e8ab7cae724e2afd62de42d6892 ;; \
      *) exit 2 ;; \
    esac && /src/build.sh /input/main.war /out "$TARGETPLATFORM" "$source_child" "$compiler_child"
FROM scratch AS artifacts
COPY --from=backport /out/ /
