# libgit2 1.5 is what git2go v34 binds to; no current distro ships it, so build it.
FROM golang:1.26-alpine3.22 AS builder

RUN apk add --no-cache build-base cmake git openssl-dev libssh2-dev zlib-dev pkgconf

ARG LIBGIT2_VERSION=1.5.2
RUN wget -qO- https://github.com/libgit2/libgit2/archive/refs/tags/v${LIBGIT2_VERSION}.tar.gz | tar xz -C /tmp && \
    cmake -S /tmp/libgit2-${LIBGIT2_VERSION} -B /tmp/libgit2-build \
        -DCMAKE_BUILD_TYPE=Release -DCMAKE_INSTALL_PREFIX=/usr/local \
        -DBUILD_TESTS=OFF -DBUILD_CLI=OFF -DUSE_SSH=ON -DUSE_HTTPS=OpenSSL \
        -DREGEX_BACKEND=builtin && \
    cmake --build /tmp/libgit2-build -j"$(nproc)" && \
    cmake --install /tmp/libgit2-build && \
    rm -rf /tmp/libgit2-*

ENV PKG_CONFIG_PATH=/usr/local/lib/pkgconfig GOTOOLCHAIN=local

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
RUN go build -o git-subdir-filter ./cmd/git-subdir-filter

FROM alpine:3.22
RUN apk add --no-cache openssh-client libssh2 libssl3 zlib
COPY --from=builder /usr/local/lib/libgit2.so* /usr/local/lib/

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
