# Build Gbrea in a stock Go builder container
FROM golang:1.12-alpine as builder

RUN apk add --no-cache make gcc musl-dev linux-headers git

ADD . /go-breadiuma
RUN cd /go-breadiuma && make gbrea

# Pull Gbrea into a second stage deploy alpine container
FROM alpine:latest

RUN apk add --no-cache ca-certificates
COPY --from=builder /go-breadiuma/build/bin/gbrea /usr/local/bin/

EXPOSE 7464 7465 33760 33760/udp
ENTRYPOINT ["gbrea"]
