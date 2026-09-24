FROM golang:1.25-alpine AS build
ARG GOPROXY=https://proxy.golang.org,direct
ENV GOPROXY=$GOPROXY
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/sinthmux-hub ./apps/hub
RUN for platform in darwin linux; do for arch in amd64 arm64; do \
      CGO_ENABLED=0 GOOS=$platform GOARCH=$arch go build -trimpath -o /out/sinthmux-connector-$platform-$arch ./apps/connector || exit 1; \
    done; done

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /out/sinthmux-hub /sinthmux-hub
COPY --from=build /out/sinthmux-connector-* /downloads/
ENV SINTHMUX_CONNECTOR_DOWNLOAD_DIR=/downloads
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/sinthmux-hub"]
