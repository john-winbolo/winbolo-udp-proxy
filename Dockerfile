FROM golang:1.22-alpine AS builder

WORKDIR /build

COPY src/go.mod src/go.sum ./
RUN go mod download

COPY src/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o proxy .

# ---

FROM alpine:3.19

RUN apk add --no-cache ca-certificates

COPY --from=builder /build/proxy /proxy

EXPOSE 8085

ENTRYPOINT ["/proxy"]
