FROM --platform=$BUILDPLATFORM node:22-alpine AS frontend
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS backend
ARG TARGETOS
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY migrations ./migrations
COPY web/embed.go ./web/embed.go
COPY --from=frontend /src/web/dist ./web/dist
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/agent ./cmd/agent && \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/dbmigrate ./cmd/dbmigrate

FROM alpine:3.22
RUN apk add --no-cache ca-certificates && adduser -D -u 10001 relay && mkdir /data && chown relay /data
COPY --from=backend /out/server /usr/local/bin/embyrelay
COPY --from=backend /out/agent /usr/local/bin/embyrelay-agent
COPY --from=backend /out/dbmigrate /usr/local/bin/embyrelay-db-migrate
USER relay
ENV RELAY_LISTEN=0.0.0.0:6689 RELAY_DATA_DIR=/data
EXPOSE 6689
HEALTHCHECK --interval=30s --timeout=5s CMD wget -q -O /dev/null http://127.0.0.1:6689/healthz || exit 1
ENTRYPOINT ["/usr/local/bin/embyrelay"]
