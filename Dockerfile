ARG GOLANG_VERSION

FROM golang:${GOLANG_VERSION}-alpine as builder

RUN apk update \
    && apk upgrade && apk add git

WORKDIR /go/src/flouret.io/update-route53

COPY . .

RUN go get -v .
RUN CGO_ENABLED=0 go build -a -installsuffix cgo -ldflags "-s -w" -o update-route53 .

FROM alpine:latest as alpine
RUN apk update && apk upgrade && apk add --no-cache ca-certificates
RUN update-ca-certificates
RUN adduser -D -h / -H -s /sbin/nologin -u 10001 -g "" update-route53

FROM scratch
COPY --from=alpine /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=alpine /etc/passwd /etc/group /etc/
COPY --from=builder /go/src/flouret.io/update-route53/update-route53 /
USER update-route53:update-route53
CMD ["/update-route53"]
