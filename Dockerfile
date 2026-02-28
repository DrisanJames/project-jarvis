FROM alpine:3.20

RUN apk --no-cache add ca-certificates tzdata wget && \
    addgroup -S ignite && adduser -S ignite -G ignite

WORKDIR /app

COPY build/server .
COPY config/config.yaml ./config/config.yaml
COPY migrations ./migrations
COPY web/dist ./web/dist

RUN mkdir -p /app/data && chown -R ignite:ignite /app

USER ignite

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s --start-period=10s --retries=3 \
  CMD wget --no-verbose --tries=1 --spider http://localhost:8080/health || exit 1

CMD ["./server"]
