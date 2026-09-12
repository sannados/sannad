# syntax=docker/dockerfile:1

FROM golang:1.25-alpine AS builder

ARG BINARY=sannad-kernel

# CGO is required for the SQLite driver (dev/test builds).
# Production deployments using PostgreSQL do not need CGO but
# the same image must support both, so we keep it enabled.
RUN apk add --no-cache gcc musl-dev

WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=1 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/sannad \
    ./cmd/${BINARY}

# ---- runtime stage ----
FROM alpine:3.21

RUN apk add --no-cache ca-certificates tzdata && \
    addgroup -S nonroot && \
    adduser -S nonroot -G nonroot

WORKDIR /app
COPY --from=builder /out/sannad ./sannad

USER nonroot:nonroot
EXPOSE 8080 8081

ENTRYPOINT ["./sannad"]
