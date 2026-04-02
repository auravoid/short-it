FROM golang:alpine AS builder

RUN apk add --no-cache ca-certificates

WORKDIR /app

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-w -s" -a -installsuffix cgo -o short-it ./cmd/short-it

FROM scratch

COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/

COPY --from=builder /app/short-it /short-it

EXPOSE 8080
EXPOSE 8090

ENV DB_PATH=/data/short-it.db

CMD ["/short-it"]