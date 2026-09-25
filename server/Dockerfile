# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/server ./cmd/server \
 && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/server /server
# Named volumes inherit this ownership on first mount, so the nonroot user can write.
COPY --from=build --chown=65532:65532 /out/data /data
ENV DATA_DIR=/data LISTEN_ADDR=:8080
EXPOSE 8080
VOLUME /data
USER 65532:65532
ENTRYPOINT ["/server"]
