FROM --platform=$BUILDPLATFORM golang:1.26 AS build
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=docker
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags "-s -w -X main.version=$VERSION" -o /tailon-ng .

# Final stage: distroless "static"
FROM gcr.io/distroless/static-debian13
COPY --from=build /tailon-ng /usr/local/bin/tailon-ng
EXPOSE 8080
# Paths to serve are given as arguments: `docker run … <image> /var/log`.
ENTRYPOINT ["tailon-ng"]
