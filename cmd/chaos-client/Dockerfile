FROM golang:1.25-alpine AS builder

WORKDIR /workspace

# Copy go mod files
COPY go.mod go.sum ./
RUN go mod download

# Copy source
COPY api/ api/
COPY test/chaos/ test/chaos/
COPY cmd/chaos-client/ cmd/chaos-client/

# Build
RUN CGO_ENABLED=0 GOOS=linux go build -a -o chaos-client ./cmd/chaos-client

# Runtime image
FROM alpine:3.19

RUN apk --no-cache add ca-certificates

WORKDIR /
COPY --from=builder /workspace/chaos-client .

ENTRYPOINT ["/chaos-client"]
