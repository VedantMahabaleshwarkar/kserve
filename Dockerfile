# Build the manager binary
FROM registry.access.redhat.com/ubi8/go-toolset:1.19 as builder

# Copy in the go src
WORKDIR /go/src/github.com/kserve/kserve
COPY go.mod  go.mod
COPY go.sum  go.sum

RUN go mod download

COPY cmd/    cmd/
COPY pkg/    pkg/

# Build
RUN CGO_ENABLED=0 GOOS=linux GOFLAGS=-mod=mod go build -a -o manager ./cmd/manager

# Use distroless as minimal base image to package the manager binary
FROM registry.access.redhat.com/ubi8/ubi-micro:8.6 
COPY third_party/ /third_party/
COPY --from=builder /go/src/github.com/kserve/kserve/manager /
ENTRYPOINT ["/manager"]