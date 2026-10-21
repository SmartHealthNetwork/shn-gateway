# STANDALONE build of the PUBLIC gateway binary (cmd/gateway within this module).
# Build context = THIS module (gateway/) — a partner clones only shn-gateway, no sdk/
# sibling. shn-sdk resolves from the public Go proxy via the go.mod require (pinned
# version tracks gateway/go.mod), so there is no local replace. Our cloud deploy
# builds this exact artifact (dogfood); the local separated smoke stack builds the
# same binary from the workspace instead (deploy/multiprocess/Dockerfile, gateway-ws).
# The builder runs on the build's own platform and compiles for the target one, so one build
# makes the linux/amd64 and linux/arm64 images of the published release without emulation.
FROM --platform=$BUILDPLATFORM golang:1.26.10@sha256:5c3f3f28a273c9b61698b2d34f18375929790ab1f1d3835d032d21e2b31eae0c AS builder
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/gateway ./cmd/gateway

# Distroless static NONROOT (uid/gid 65532): no shell (keep healthchecks binary/none),
# CA certs for outbound HTTPS. The runtime reads its key bundle from the mounted
# /etc/shn volume — tools/materialize chowns the bundle dir+secrets to gid 65532 so the
# nonroot runtime can traverse+read them (keys stay non-world-readable; AI-8 custody).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/gateway /gateway
USER 65532:65532
ENTRYPOINT ["/gateway"]
