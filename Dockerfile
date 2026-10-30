FROM --platform=$BUILDPLATFORM golang:1.26.10-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go routes.txt ./
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /tmdb-relay .

FROM alpine:3.23
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 relay
COPY --from=build /tmdb-relay /usr/local/bin/tmdb-relay
USER relay
WORKDIR /app
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/tmdb-relay"]
