# Central portal image. One binary, three subcommands (server|worker|migrate)
# selected by the container's args — see cmd/portal. The image also carries the
# cross-compiled pi-agent binary at /agent/pi-agent-armv6, which the server
# hands out for fleet self-updates (AGENT_BINARY_PATH).
FROM golang:1.26 AS build
WORKDIR /src

ARG VERSION=dev
ENV LDFLAGS="-s -w -X github.com/dangerweenie/resin-print-portal/internal/buildinfo.Version=${VERSION}"

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="${LDFLAGS}" -o /out/portal ./cmd/portal
RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm GOARM=6 \
        go build -trimpath -ldflags="${LDFLAGS}" -o /out/pi-agent-armv6 ./cmd/pi-agent

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/portal /portal
COPY --from=build /out/pi-agent-armv6 /agent/pi-agent-armv6
# Default to the API server; Helm's worker and migrate objects override args.
ENTRYPOINT ["/portal"]
CMD ["server"]
