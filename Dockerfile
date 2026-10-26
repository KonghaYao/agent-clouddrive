FROM golang:1.25.0-bookworm@sha256:81dc45d05a7444ead8c92a389621fafabc8e40f8fd1a19d7e5df14e61e98bc1a AS go-build
ARG GOPROXY
ENV GOPROXY=${GOPROXY:-https://proxy.golang.org,direct}
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
COPY db ./db
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/api ./cmd/api \
 && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/worker ./cmd/worker \
 && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/maintenance ./cmd/maintenance

FROM node:22.14.0-alpine3.21@sha256:9bef0ef1e268f60627da9ba7d7605e8831d5b56ad07487d24d1aa386336d1944 AS web-build
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci
COPY web ./
RUN npm run build

FROM nginx:1.28.0-alpine@sha256:30f1c0d78e0ad60901648be663a710bdadf19e4c10ac6782c235200619158284 AS gateway
COPY --from=web-build /src/web/dist /usr/share/nginx/html
COPY deploy/compose/nginx.conf /etc/nginx/nginx.conf
EXPOSE 8080
HEALTHCHECK --interval=15s --timeout=3s CMD wget -q -O /dev/null http://127.0.0.1:8080/healthz || exit 1

FROM alpine:3.21.3@sha256:a8560b36e8b8210634f77d9f7f9efd7ffa463e380b75e2e74aff4511df3ef88c AS app
RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -g 10001 app && adduser -D -H -u 10001 -G app app \
 && mkdir -p /data/artifacts && chown app:app /data/artifacts
WORKDIR /app
COPY --from=go-build /out/ /app/
COPY db/migrations /app/db/migrations
COPY deploy/compose/app-entrypoint.sh /app/entrypoint.sh
USER 10001:10001
EXPOSE 8080 8081
ENTRYPOINT ["/bin/sh", "/app/entrypoint.sh"]
CMD ["/app/api"]
