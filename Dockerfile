FROM golang:alpine as builder

# Update and grab build packages
RUN apk update && \
    apk add --no-cache openssh-client git alpine-sdk libgit2-dev

# Build the tool
RUN mkdir -p /app /go
WORKDIR /app
COPY git-subdir-filter.go .
RUN GOPATH=/go go get -d -v ./...
RUN GOPATH=/go go build -o git-subdir-filter .

FROM alpine:3.8
# Update and get runtime packages
RUN apk update && \
    apk add --no-cache openssh-client libgit2>=0.27

RUN mkdir /root/.ssh/
COPY id_rsa.pub /root/.ssh/
COPY id_rsa /root/.ssh/
RUN chmod 400 /root/.ssh/id_rsa
RUN chmod 400 /root/.ssh/id_rsa.pub

RUN eval $(ssh-agent -s)

# Accept the github.west domain
RUN touch /root/.ssh/known_hosts
RUN ssh-keyscan github.west.isilon.com >> /root/.ssh/known_hosts

# copy binary
RUN mkdir /app
COPY --from=builder /app/git-subdir-filter /app

ARG SOURCE_BRANCH
ARG SOURCE_REPO
ARG TARGET_REPO
ARG FILTER_DIR
ARG WORKDIR

WORKDIR /git

# run in shell form for variable substitution
ENTRYPOINT /app/git-subdir-filter \
    -source-repo=$SOURCE_REPO \
    -source-branch=$SOURCE_BRANCH \
    -target-repo=$TARGET_REPO \
    -filter-dir=$FILTER_DIR
