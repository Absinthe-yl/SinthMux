FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/sinthmux-login-broker ./apps/login-broker

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/sinthmux-login-broker /sinthmux-login-broker
USER nonroot:nonroot
EXPOSE 8091
ENTRYPOINT ["/sinthmux-login-broker"]
