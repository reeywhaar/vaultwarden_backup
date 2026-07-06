FROM golang:1.26-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -o /backup .

FROM alpine:latest
RUN apk add --no-cache sqlite 7zip
COPY --from=builder /backup /backup
COPY --chmod=0755 backup_loop.sh /backup_loop.sh
CMD ["/backup_loop.sh"]
