# Build from the repo root:
#   docker build -f examples/fullstack/Dockerfile -t support-desk .
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags=-s -o /support-desk ./examples/fullstack

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /support-desk /support-desk
ENV PORT=8080
EXPOSE 8080
ENTRYPOINT ["/support-desk", "-data", "/data"]
