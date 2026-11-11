# STANDALONE build of the PUBLIC gateway binary (cmd/gateway within this module).
# Build context = THIS module (gateway/) — a partner clones only shn-gateway, no sdk/
# sibling. shn-sdk resolves from the public Go proxy via the go.mod require (pinned
# version tracks gateway/go.mod), so there is no local replace. Our cloud deploy
# builds this exact artifact (dogfood); the local separated smoke stack builds the
# same binary from the workspace instead (deploy/multiprocess/Dockerfile, gateway-ws).
# The builder runs on the build's own platform and compiles for the target one, so one build
# makes the linux/amd64 and linux/arm64 images of the published release without emulation.
# GATEWAY_VERSION stamps the build's version (internal/buildversion): a release's tag, or a main
# build's pseudo-version. Unset, the binary falls back to the module version Go records, else
# v0.0.0-unstamped.
# The Go modules a build may pass as the gomodules build context (compose
# `additional_contexts`): a directory laid out as a module proxy, which a gate
# restores from its own cache. With GO_MODULES_PROXY=file:///gomodules,off, as in
# the gates, the build reads modules only from there, and go.sum judges them as it
# judges a download. With neither, as in a local or a deploy build, the context is
# empty and Go uses its default proxy.
FROM scratch AS gomodules

FROM --platform=$BUILDPLATFORM golang:1.26.11@sha256:7af425da11f4fc1a9308eb704369e3f6d1c44e35bac0f874a59ade3f20fdc07e AS builder
ARG TARGETOS TARGETARCH
WORKDIR /src
ARG GO_MODULES_PROXY=
ENV GOPROXY=$GO_MODULES_PROXY
COPY --from=gomodules / /gomodules/
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# GATEWAY_VERSION is declared here, after the module download, so a new version does not miss that
# layer's cache. The -X target sits in a variable, so the go build line names no package
# the platform's deploy-scope check would read as one the image builds.
ARG GATEWAY_VERSION=
RUN stamp=; [ -z "$GATEWAY_VERSION" ] || stamp="-X github.com/SmartHealthNetwork/shn-gateway/internal/buildversion.stamp=$GATEWAY_VERSION"; \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w $stamp" -o /out/gateway ./cmd/gateway

# Distroless static NONROOT (uid/gid 65532): no shell (keep healthchecks binary/none),
# CA certs for outbound HTTPS. The runtime reads its key bundle from the mounted
# /etc/shn volume — the platform's bundle materializer chowns the bundle dir+secrets to gid 65532 so the
# nonroot runtime can traverse+read them (keys stay non-world-readable; AI-8 custody).
FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
COPY --from=builder /out/gateway /gateway
# The licences of the modules the binary links, and the gateway's own.
COPY THIRD_PARTY_NOTICES.md LICENSE /licenses/
USER 65532:65532
ENTRYPOINT ["/gateway"]
