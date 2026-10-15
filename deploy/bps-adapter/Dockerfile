FROM golang:1.27.0-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -tags=http2legacy -trimpath -ldflags="-s -w" -o /out/bps-adapter ./cmd/server
FROM alpine:3.21
RUN apk add --no-cache ca-certificates && addgroup -g 10001 bps && adduser -D -H -u 10001 -G bps bps
COPY --from=build /out/bps-adapter /usr/local/bin/bps-adapter
COPY UPSTREAM.md THIRD_PARTY_NOTICES.md CPA-MIT-LICENSE /usr/share/bps-adapter/
COPY licenses/ /usr/share/bps-adapter/licenses/
USER 10001:10001
EXPOSE 8000
HEALTHCHECK --interval=15s --timeout=3s --start-period=10s CMD wget -q -O /dev/null http://127.0.0.1:8000/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/bps-adapter"]
