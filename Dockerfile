FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /backup .

# Pin the official CLI; no custom S3 SDK or transfer implementation.
FROM amazon/aws-cli:2.39.2
COPY --from=build /backup /usr/local/bin/file-s3-backup
COPY aws-config /etc/file-s3-backup/aws-config
ENV AWS_CONFIG_FILE=/etc/file-s3-backup/aws-config \
    AWS_EC2_METADATA_DISABLED=true \
    AWS_REQUEST_CHECKSUM_CALCULATION=WHEN_REQUIRED \
    AWS_RESPONSE_CHECKSUM_VALIDATION=WHEN_REQUIRED
WORKDIR /app
USER 1000:1002
ENTRYPOINT ["/usr/local/bin/file-s3-backup"]
