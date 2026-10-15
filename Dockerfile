FROM golang:1.26.9-alpine@sha256:3082400e369fa24d5fc60bca20edab3f6d604e0c5a690ec66b295eff4dd87ade AS builder

RUN apk add --no-cache ca-certificates tzdata
WORKDIR /build

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux \
    go build -trimpath -ldflags="-w -s" -o notification-api ./cmd/notification

RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-w -s" -o dsr-inventory ./cmd/dsr-inventory

FROM gcr.io/distroless/static-debian12:nonroot@sha256:f5b485ea962d9bd1186b2f6b3a061191539b905b82ec395de78cbfae51f20e35

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=builder /usr/share/zoneinfo /usr/share/zoneinfo
COPY --from=builder --chown=nonroot:nonroot /build/notification-api /app/notification-api

COPY --from=builder --chown=nonroot:nonroot /build/dsr-inventory /app/dsr-inventory

WORKDIR /app
USER nonroot:nonroot
EXPOSE 8081

ENTRYPOINT ["/app/notification-api"]
CMD ["api"]
