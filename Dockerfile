FROM golang:1.27-alpine AS builder

WORKDIR /build
COPY --link go.mod go.sum ./
RUN go mod download

COPY --link cmd/ cmd/
COPY --link internal/ internal/

RUN CGO_ENABLED=0 go build -o /out/ingest ./cmd/ingest \
 && CGO_ENABLED=0 go build -o /out/query ./cmd/query

FROM gcr.io/distroless/static-debian12:nonroot

COPY --link --from=builder /out/ingest /ingest
COPY --link --from=builder /out/query /query

# Default binary; each Nais app overrides with spec.command.
ENTRYPOINT ["/query"]
