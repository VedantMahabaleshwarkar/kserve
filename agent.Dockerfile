# Build the inference-agent binary
FROM registry.access.redhat.com/ubi8/go-toolset:1.21 as builder

# Copy in the go src
WORKDIR /go/src/github.com/kserve/kserve
COPY go.mod  go.mod
COPY go.sum  go.sum

RUN go mod download

COPY pkg/    pkg/
COPY cmd/    cmd/

# Build
USER root
RUN CGO_ENABLED=0 GOOS=linux GOFLAGS=-mod=mod go build -a -o agent ./cmd/agent

# Copy the inference-agent into a thin image
FROM registry.access.redhat.com/ubi8/ubi-micro:latest
COPY third_party/ third_party/
WORKDIR /ko-app
COPY --from=builder /go/src/github.com/kserve/kserve/agent /ko-app/
USER 65531:65531

ENTRYPOINT ["/ko-app/agent"]