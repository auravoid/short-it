FROM golang:1.24-alpine AS builder

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -o short-it ./cmd/short-it
FROM alpine:latest

RUN apk --no-cache add ca-certificates

WORKDIR /root/

COPY --from=builder /app/short-it .
RUN mkdir -p /data

EXPOSE 8080
ENV DB_PATH=/data/short-it.db

CMD ["./short-it"] 