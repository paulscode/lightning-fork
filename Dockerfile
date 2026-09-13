# Lightning Fork release image: lnd and lncli for the Bitcoin BLAKE2b chain.
#
# Built from a pinned git ref of this repository and of the btcd fork that
# go.mod pins, so the image records exactly what it runs. Pass a tag, branch
# or commit with --build-arg checkout=...; the default is the blake2b branch.
#
# If you change the Go version here please also update GO_VERSION in Makefile.
FROM golang:1.26.6-alpine AS builder

# Force Go to use the cgo based DNS resolver. This is required to ensure DNS
# queries required to connect to linked containers succeed.
ENV GODEBUG netdns=cgo

ARG checkout="blake2b"
ARG git_url="https://github.com/paulscode/lightning-fork"

# Install dependencies and build the binaries. The module path is upstream's
# (github.com/lightningnetwork/lnd); go.mod replaces btcd with the fork.
RUN apk add --no-cache --update alpine-sdk \
    git \
    make \
    gcc \
&&  git clone $git_url /go/src/github.com/lightningnetwork/lnd \
&&  cd /go/src/github.com/lightningnetwork/lnd \
&&  git checkout $checkout \
&&  git rev-parse HEAD > /lightning-fork-commit \
&&  go list -m -f '{{.Replace.Path}} {{.Replace.Version}}' github.com/btcsuite/btcd > /btcd-blake2b-version \
&&  make release-install

# Start a new, final image.
FROM alpine AS final

# Define a root volume for data persistence.
VOLUME /root/.lnd

# Add utilities for quality of life and SSL-related reasons. We also require
# wget and gpg for the signature verification script.
RUN apk --no-cache add \
    bash \
    jq \
    ca-certificates \
    gnupg \
    wget

# Copy the binaries from the builder image.
COPY --from=builder /go/bin/lncli /bin/
COPY --from=builder /go/bin/lnd /bin/
COPY --from=builder /go/src/github.com/lightningnetwork/lnd/scripts/verify-install.sh /
COPY --from=builder /go/src/github.com/lightningnetwork/lnd/scripts/keys/* /keys/

# Record what this image runs: the fork commit and the btcd fork version, so
# a running container can say exactly which code it carries.
COPY --from=builder /lightning-fork-commit /etc/lightning-fork-commit
COPY --from=builder /btcd-blake2b-version /etc/btcd-blake2b-version

# Store the SHA256 hash of the binaries that were just produced for later
# verification.
RUN sha256sum /bin/lnd /bin/lncli > /shasums.txt \
  && cat /shasums.txt

# Expose lnd ports (p2p, rpc).
EXPOSE 9735 10009

# Specify the start command and entrypoint as the lnd daemon.
ENTRYPOINT ["lnd"]
