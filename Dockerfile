FROM golang:1.20.0

WORKDIR /go/src/app

COPY go.mod go.sum zappi.go ./

RUN CGO_ENABLED=0 GOOS=linux go build -o zappi-exporter .

FROM alpine:3.17.1

COPY --from=0 /go/src/app/zappi-exporter /usr/bin/zappi-exporter

USER nobody

ENTRYPOINT ["/usr/bin/zappi-exporter"]

EXPOSE 8080
