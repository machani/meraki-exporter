# syntax=docker/dockerfile:1
# Build stage: matches the toolchain pinned in go.mod.
FROM golang:1.26.5 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/meraki-exporter ./cmd/meraki-exporter

# Final stage: the binary is static and writes nothing, so scratch suffices.
# Only CA certificates are needed (TLS to the Meraki API).
FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=build /out/meraki-exporter /meraki-exporter
USER 65534:65534
EXPOSE 9822
ENTRYPOINT ["/meraki-exporter"]
