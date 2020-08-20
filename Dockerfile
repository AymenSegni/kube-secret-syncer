FROM golang:1.15.0

WORKDIR /go/src/k8s-secret-syncer

COPY . . 

RUN GOOS=linux go build .


FROM debian:stretch-slim
COPY --from=0 /go/src/k8s-secret-syncer/k8s-secret-syncer .
ENTRYPOINT [ "/k8s-secret-syncer" ]
