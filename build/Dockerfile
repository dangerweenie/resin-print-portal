# Central portal image. One binary, three subcommands (server|worker|migrate)
# selected by the container's args — see cmd/portal.
FROM golang:1.26 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/portal ./cmd/portal

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/portal /portal
# Default to the API server; Helm's worker and migrate objects override args.
ENTRYPOINT ["/portal"]
CMD ["server"]
