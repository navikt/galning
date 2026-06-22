FROM golang:1.26-alpine AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY main.go .
COPY internal/ internal/

RUN CGO_ENABLED=0 go build -o /galning

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /galning /galning

ENTRYPOINT ["/galning"]
