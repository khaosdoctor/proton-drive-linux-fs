FROM golang:1.23-alpine AS builder

RUN apk add --no-cache fuse3-dev musl-dev gcc

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=1 go build -o /proton-drive-fs ./cmd/proton-drive-fs

FROM alpine:3.21
RUN apk add --no-cache fuse3 ca-certificates
COPY --from=builder /proton-drive-fs /usr/local/bin/proton-drive-fs

ENTRYPOINT ["proton-drive-fs"]
