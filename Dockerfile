# syntax=docker/dockerfile:1
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/grabr ./cmd/grabr

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/grabr /grabr
USER nonroot:nonroot
ENV GRABR_DATA_DIR=/data
ENV GRABR_PORT=8080
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["/grabr"]
