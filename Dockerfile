FROM golang:1.26.4-alpine AS builder

WORKDIR /build
COPY go.mod go.sum ./
RUN go mod download

COPY main.go .
COPY internal/ .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -o /galning .


FROM gcr.io/distroless/static-debian12

COPY --from=builder /galning /galning
USER nonroot:nonroot

ENTRYPOINT ["/galning"]
