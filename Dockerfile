# syntax=docker/dockerfile:1.7

# ---- build stage ----
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/grimoire ./cmd/grimoire

# ---- runtime stage ----
FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata wget && \
    adduser -D -u 10001 grimoire
WORKDIR /app
COPY --from=build /out/grimoire /app/grimoire
RUN mkdir -p /data && chown -R grimoire:grimoire /data /app
USER grimoire
ENV GRIMOIRE_ADDR=:8080 \
    GRIMOIRE_DB=/data/grimoire.db
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD wget -qO- http://127.0.0.1:8080/healthz >/dev/null 2>&1 || exit 1
ENTRYPOINT ["/app/grimoire"]
CMD ["serve"]
