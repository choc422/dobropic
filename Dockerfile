FROM golang:1.25-alpine AS builder

RUN apk add --no-cache gcc musl-dev

WORKDIR /build
COPY go.mod go.sum ./
ENV GOTOOLCHAIN=auto
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=1 go build -o bot .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates

WORKDIR /app
COPY --from=builder /build/bot .
COPY xray/xray /usr/local/bin/xray
RUN chmod +x /usr/local/bin/xray

ENV DATA_DIR=/app/data

VOLUME ["/app/data"]

CMD ["./bot"]
