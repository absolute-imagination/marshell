# Multi-stage build for the self-hosted Marshell Network relay.
FROM golang:1.23-alpine AS build
WORKDIR /src
RUN apk add --no-cache ca-certificates git
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/network ./cmd/network

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=build /out/network /app/network
ENV LISTEN_ADDR=0.0.0.0
ENV PORT=8080
ENV PUBLIC_NETWORK_URL=http://localhost:8080
EXPOSE 8080
USER nobody
ENTRYPOINT ["/app/network"]
