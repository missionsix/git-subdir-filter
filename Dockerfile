# Go toolchain only; its static binaries run on the older Alpine below.
FROM golang:1.22-alpine AS go

# Alpine 3.8 ships libgit2 0.27, which git2go v27 requires.
FROM alpine:3.8 AS builder

# Update and grab build packages
RUN apk add --no-cache openssh-client git alpine-sdk 'libgit2-dev~=0.27'

COPY --from=go /usr/local/go /usr/local/go
ENV PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=local

# Build the tool
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
RUN go build -o git-subdir-filter ./cmd/git-subdir-filter

FROM alpine:3.8
# Update and get runtime packages
RUN apk add --no-cache openssh-client 'libgit2~=0.27'

RUN mkdir /root/.ssh/
COPY id_rsa.pub /root/.ssh/
COPY id_rsa /root/.ssh/
RUN chmod 400 /root/.ssh/id_rsa
RUN chmod 400 /root/.ssh/id_rsa.pub

RUN eval $(ssh-agent -s)

# XXX Accept the github and gitlab domains
RUN touch /root/.ssh/known_hosts
RUN ssh-keyscan github.com >> /root/.ssh/known_hosts
RUN ssh-keyscan gitlab.com >> /root/.ssh/known_hosts

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
