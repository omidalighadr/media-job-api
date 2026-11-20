FROM golang:1.27.4-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -o /media-job-api ./cmd/media-job-api

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends ffmpeg ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && groupadd --gid 10001 mediajob \
    && useradd --uid 10001 --gid mediajob --no-create-home mediajob \
    && mkdir /data && chown mediajob:mediajob /data
COPY --from=build /media-job-api /usr/local/bin/media-job-api
USER 10001:10001
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["media-job-api"]
CMD ["-listen", "0.0.0.0:8080", "-data", "/data"]
