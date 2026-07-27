# syntax=docker/dockerfile:1.4

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder

RUN apk add --no-cache ca-certificates tzdata
WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH

RUN --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -ldflags="-w -s" -o notification-api ./cmd/notification

FROM --platform=$TARGETPLATFORM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder --chown=nonroot:nonroot /build/notification-api /app/notification-api

WORKDIR /app
USER nonroot:nonroot
EXPOSE 8081

ENTRYPOINT ["/app/notification-api"]
CMD ["api"]
