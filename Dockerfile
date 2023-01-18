FROM golang:1.19-alpine as builder

WORKDIR /workspace

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

COPY / /workspace

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 GO111MODULE=on go build

FROM alpine

COPY --from=builder /workspace/pulumi-helper /usr/bin/ph

# Run the binary.
ENTRYPOINT ["/usr/bin/ph"]
