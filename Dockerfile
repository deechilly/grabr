# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/grabr ./cmd/grabr
# Pre-create /data so the runtime stage can COPY it with the right ownership.
# Docker copies the image's /data contents (and ownership) into a fresh named
# volume on first mount, which is what makes the volume writable by nonroot.
RUN mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/grabr /grabr
COPY --from=build --chown=nonroot:nonroot /out/data /data
USER nonroot:nonroot
ENV GRABR_DATA_DIR=/data
ENV GRABR_PORT=8080
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/grabr"]
