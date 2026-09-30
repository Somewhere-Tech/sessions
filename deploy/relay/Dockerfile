FROM golang:1.24-alpine AS build
WORKDIR /src/runtime
COPY runtime/go.mod runtime/go.sum ./
RUN go mod download
COPY runtime/ ./
RUN CGO_ENABLED=0 GOFLAGS=-buildvcs=false go build -trimpath -ldflags="-s -w" -o /out/sessions-relay ./cmd/sessions-relay

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/sessions-relay /sessions-relay
USER 65532:65532
EXPOSE 8899
ENTRYPOINT ["/sessions-relay"]
