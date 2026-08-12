FROM golang:1.26-alpine AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY cmd/ cmd/
COPY internal/ internal/

RUN CGO_ENABLED=0 go build -o /out/ingest ./cmd/ingest \
 && CGO_ENABLED=0 go build -o /out/query ./cmd/query

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/ingest /ingest
COPY --from=builder /out/query /query

# Default binary; each NAIS app overrides with spec.command.
ENTRYPOINT ["/query"]
