FROM golang:1.10
LABEL maintainer="Benjamin R. Haskell <docker@benizi.com>"
WORKDIR /go
COPY . /go/src/github.com/benizi/dotenv
RUN go get -d -v github.com/mattn/go-shellwords
RUN go install github.com/benizi/dotenv
ENTRYPOINT ["/go/bin/dotenv"]
