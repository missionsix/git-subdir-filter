# libgit2 1.5 is what git2go v34 binds to; no current distro ships it, so build it.
FROM golang:1.26-alpine3.22 AS builder

RUN apk add --no-cache build-base cmake git git-subtree openssl-dev libssh2-dev zlib-dev pkgconf

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
COPY internal/ internal/
RUN go build -trimpath -o git-subdir-filter ./cmd/git-subdir-filter

# docker build --target test .   runs vet and the test suite.
FROM builder AS test
RUN go vet ./... && go test -race ./...

FROM alpine:3.22
RUN apk add --no-cache ca-certificates libssh2 libssl3 zlib && \
    adduser -D -u 10001 filter && \
    mkdir /work && chown filter:filter /work
COPY --from=builder /usr/local/lib/libgit2.so* /usr/local/lib/
COPY --from=builder /app/git-subdir-filter /usr/local/bin/git-subdir-filter

# Credentials are never baked in: mount ~/.ssh or forward SSH_AUTH_SOCK at run time.
USER filter
ENV WORKDIR=/work/repo
VOLUME /work
ENTRYPOINT ["/usr/local/bin/git-subdir-filter"]
