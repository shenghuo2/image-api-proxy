FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /plana-novelai-proxy ./cmd/plana-novelai-proxy

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /plana-novelai-proxy /plana-novelai-proxy
USER 65532:65532
EXPOSE 8787
ENTRYPOINT ["/plana-novelai-proxy"]
