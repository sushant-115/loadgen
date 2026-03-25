# Stage 1: Build
FROM golang:1.25.0-alpine AS builder

RUN apk add --no-cache git ca-certificates tzdata build-base

WORKDIR /src

# Cache dependency downloads
COPY go.mod go.sum ./
RUN mkdir -p /go/pkg/mod && \
    go mod download -x || true

COPY . .

ARG SERVICE_NAME

# Build the service binary
RUN CGO_ENABLED=1 GOOS=linux GOARCH=amd64 \
    go build -ldflags="-s -w -extldflags '-static'" \
    -o /out/${SERVICE_NAME} ./cmd/${SERVICE_NAME}

# Stage 2: Runtime
FROM alpine:3.19

RUN apk add --no-cache ca-certificates tzdata curl

ARG SERVICE_NAME

COPY --from=builder /out/${SERVICE_NAME} /app/${SERVICE_NAME}

EXPOSE 8080

CMD ["/app/${SERVICE_NAME}"]
