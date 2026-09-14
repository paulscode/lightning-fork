# Lightning Fork release image: lnd and lncli for the Bitcoin BLAKE2b chain.
#
# Built from a pinned git ref of this repository and of the btcd fork that
# go.mod pins, so the image records exactly what it runs. Pass a tag, branch
# or commit with --build-arg checkout=...; the default is the blake2b branch.
#
# If you change the Go version here please also update GO_VERSION in Makefile.
#
# The builder runs on the build machine's own architecture and cross-compiles
# for the target (the release build has no cgo), so a multi-platform build
# does not run the Go compiler under emulation.
FROM --platform=$BUILDPLATFORM golang:1.26.6-alpine AS builder

ARG checkout="blake2b"
ARG git_url="https://github.com/paulscode/lightning-fork"
ARG TARGETOS
ARG TARGETARCH

# Install dependencies and build the binaries. The module path is upstream's
# (github.com/lightningnetwork/lnd); go.mod replaces btcd with the fork. Go
# installs a cross-compiled binary under a per-platform directory, so it is
# moved to where the final stage looks.
RUN apk add --no-cache --update alpine-sdk \
    git \
    make \
    gcc \
&&  git clone $git_url /go/src/github.com/lightningnetwork/lnd \
&&  cd /go/src/github.com/lightningnetwork/lnd \
&&  git checkout $checkout \
&&  git rev-parse HEAD > /lightning-fork-commit \
&&  go list -m -f '{{.Replace.Path}} {{.Replace.Version}}' github.com/btcsuite/btcd > /btcd-blake2b-version \
&&  GOOS=$TARGETOS GOARCH=$TARGETARCH make release-install \
&&  if [ -d /go/bin/${TARGETOS}_${TARGETARCH} ]; then \
        mv /go/bin/${TARGETOS}_${TARGETARCH}/* /go/bin/; \
    fi

# Start a new, final image.
FROM alpine AS final

# Force Go to use the cgo based DNS resolver. This is required to ensure DNS
# queries required to connect to linked containers succeed.
ENV GODEBUG netdns=cgo

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
