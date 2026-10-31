# Build from the repo root:
#   docker build -f examples/fullstack/Dockerfile -t support-desk .
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /support-desk ./examples/fullstack && mkdir /data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /support-desk /support-desk
# The data dir must be writable by the nonroot user.
COPY --from=build --chown=nonroot:nonroot /data /data
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/support-desk", "-data", "/data"]
