FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /novelai-api-proxy ./cmd/novelai-api-proxy

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /novelai-api-proxy /novelai-api-proxy
USER 65532:65532
EXPOSE 8787
ENTRYPOINT ["/novelai-api-proxy"]
