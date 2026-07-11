# STANDALONE build of the PUBLIC gateway binary (cmd/gateway within this module).
# Build context = THIS module (gateway/) — a partner clones only shn-gateway, no sdk/
# sibling. shn-sdk resolves from the public Go proxy via the go.mod require (pinned
# version tracks gateway/go.mod), so there is no local replace. Our cloud + smoke
# build this exact artifact (dogfood).
FROM golang:1.26 AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/gateway ./cmd/gateway

# Distroless static NONROOT (uid/gid 65532): no shell (keep healthchecks binary/none),
# CA certs for outbound HTTPS. The runtime reads its key bundle from the mounted
# /etc/shn volume — tools/materialize chowns the bundle dir+secrets to gid 65532 so the
# nonroot runtime can traverse+read them (keys stay non-world-readable; AI-8 custody).
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /out/gateway /gateway
USER 65532:65532
ENTRYPOINT ["/gateway"]
