FROM node:20-alpine AS web-builder

WORKDIR /src/web

COPY web/package*.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM golang:1.24-alpine AS go-builder

ARG VERSION=dev
ARG GIT_SHA=unknown
ARG BUILD_TIME=unknown
ARG IMAGE_URI=

RUN apk add --no-cache ca-certificates git

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . ./
COPY --from=web-builder /src/web/dist ./web/dist

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
  -ldflags="-s -w \
  -X 'github.com/ignite/sparkpost-monitor/internal/buildinfo.Version=${VERSION}' \
  -X 'github.com/ignite/sparkpost-monitor/internal/buildinfo.GitSHA=${GIT_SHA}' \
  -X 'github.com/ignite/sparkpost-monitor/internal/buildinfo.BuildTime=${BUILD_TIME}' \
  -X 'github.com/ignite/sparkpost-monitor/internal/buildinfo.ImageURI=${IMAGE_URI}'" \
  -o /out/server ./cmd/server

FROM alpine:3.20

RUN apk --no-cache add ca-certificates tzdata wget && \
    addgroup -S ignite && adduser -S ignite -G ignite

WORKDIR /app

COPY --from=go-builder /out/server ./server
COPY config/config.example.yaml ./config/config.yaml
COPY migrations ./migrations
COPY --from=web-builder /src/web/dist ./web/dist

RUN mkdir -p /app/data && chown -R ignite:ignite /app

USER ignite

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget -q -O /dev/null http://localhost:8080/health || exit 1

CMD ["./server"]
