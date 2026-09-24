FROM node:22-alpine AS frontend-build
WORKDIR /src/frontend
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci
COPY frontend/index.html frontend/tsconfig.json frontend/vite.config.ts ./
COPY frontend/src ./src
RUN VITE_API_BASE_URL= npm run build

FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /novelai-api-proxy ./cmd/novelai-api-proxy
RUN mkdir /data && chown 65532:65532 /data

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /novelai-api-proxy /novelai-api-proxy
COPY --from=frontend-build /src/frontend/dist /frontend
COPY --chown=65532:65532 --from=build /data /data
USER 65532:65532
ENV PROXY_FRONTEND_DIR=/frontend
EXPOSE 8787
ENTRYPOINT ["/novelai-api-proxy"]
