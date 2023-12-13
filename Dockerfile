FROM --platform=$BUILDPLATFORM golang:1.21-alpine as builder

RUN apk add --no-cache bash git

WORKDIR /workspace

# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

COPY / /workspace

ARG TARGETPLATFORM
ARG BUILDPLATFORM

RUN [ "$(uname)" = Darwin ] && system=darwin || system=linux; \
    ./ci/go-build.sh --os ${system} --arch $(echo $TARGETPLATFORM  | cut -d/ -f2)

FROM --platform=$TARGETPLATFORM alpine

RUN apk add --no-cache docker-cli

COPY --from=builder /workspace/goapp /usr/bin/pulumi-helper

# Run the binary.
ENTRYPOINT ["/usr/bin/pulumi-helper"]
