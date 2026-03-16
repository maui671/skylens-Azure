FROM golang:1.24-alpine AS builder
RUN apk add --no-cache git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /skylens-node ./cmd/skylens-node/

FROM alpine:3.21
RUN apk add --no-cache ca-certificates tzdata
RUN adduser -D -h /app skylens
COPY --from=builder /skylens-node /app/skylens-node
USER skylens
WORKDIR /app
EXPOSE 8080 8081
ENTRYPOINT ["/app/skylens-node"]
CMD ["-config", "/app/configs/config.yaml"]
