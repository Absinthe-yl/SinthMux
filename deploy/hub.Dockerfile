FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/sinthmux-hub ./apps/hub
RUN for platform in darwin linux; do for arch in amd64 arm64; do \
      CGO_ENABLED=0 GOOS=$platform GOARCH=$arch go build -trimpath -o /out/sinthmux-connector-$platform-$arch ./apps/connector || exit 1; \
    done; done

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/sinthmux-hub /sinthmux-hub
COPY --from=build /out/sinthmux-connector-* /downloads/
ENV SINTHMUX_CONNECTOR_DOWNLOAD_DIR=/downloads
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/sinthmux-hub"]
