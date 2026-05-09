FROM golang:1.26-alpine AS builder
ARG CGO_ENABLED=0
WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN go build -o server

FROM scratch
COPY --from=builder /app/server /server
COPY .env-prod /.env
ENTRYPOINT ["/server"]