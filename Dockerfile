FROM golang:1.13.0-alpine as builder
RUN apk add --no-cache build-base

ENV ADDR=0.0.0.0 GO111MODULE=on

WORKDIR /go/src/app
COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 make guerrillad

FROM alpine:latest
RUN apk add --no-cache bash ca-certificates
WORKDIR /root/

COPY --from=builder /go/src/app/guerrillad app

EXPOSE 1525

CMD ["./app", "serve"]
