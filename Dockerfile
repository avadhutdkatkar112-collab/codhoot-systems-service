FROM golang:1.22-bookworm@sha256:1f298b0c9fecdf504389a0329236f948cc04a566a2bb32337207cbaaa2f8177c AS builder
WORKDIR /build
# Copy every Go source file, not just main.go: the sandbox hardening lives in
# harden.go (portable) plus harden_unix.go / harden_other.go (per-platform).
COPY go.mod ./
COPY main.go harden.go harden_unix.go harden_other.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o /out/service .

# The native-compiler toolchains, one image for seven languages. Ada (GNAT),
# Pascal (FPC), D (GDC), Fortran (gfortran), Assembly (NASM + ld), Nim and COBOL
# (GnuCOBOL) all compile to native code and all install from the same Debian
# bookworm repositories, so they share the gcc base the way the interpreters
# share theirs. This is the toolchain set codhoot-runner-systems installs, plus
# gfortran for the Fortran entry that has no service of its own.
FROM gcc:13.2-bookworm
LABEL org.opencontainers.image.title="codhoot-systems-service"
LABEL org.opencontainers.image.description="Hardened native-compiler execution sandbox (ada, cobol, d, fortran, nim, pascal, assembly)"
RUN set -eux; \
    apt-get update; \
    apt-get install -y --no-install-recommends \
        gnat \
        fpc \
        gdc-12 \
        nasm \
        nim \
        gnucobol; \
    rm -f /usr/bin/curl /usr/bin/wget /usr/local/bin/curl /usr/local/bin/wget; \
    apt-get clean; \
    rm -rf /var/lib/apt/lists/*

# Stable names, so a registry entry never encodes a Debian version suffix
# (gdc-12 rather than gdc) that a base-image bump would silently break.
RUN set -eux; \
    ln -sf /usr/bin/gdc-12 /usr/local/bin/gdc; \
    ln -sf /usr/bin/fpc /usr/local/bin/fpc; \
    ln -sf /usr/bin/gnatmake /usr/local/bin/gnatmake; \
    ln -sf /usr/bin/nasm /usr/local/bin/nasm

# Every binary the service dispatches to must be present, and this is the same
# assertion one layer earlier: the wrapper refuses to open its port unless each
# tool it can hand a program to is on PATH, so a missing toolchain fails the
# deploy rather than the first student's submission.
#
# What a toolchain does when it runs is deliberately not exercised here. A
# compile-and-run one-liner in the build makes a typo in that one-liner fail
# the deploy for every language on the image, which is a worse trade than
# finding out on first use.
RUN set -eux; \
    for bin in gnatmake fpc gdc nasm nim cobc gfortran ld; do \
      command -v "$bin" > /dev/null; \
    done

WORKDIR /app
COPY --from=builder /out/service /app/service
RUN chmod 0755 /app/service && rm -f go.mod main.go harden.go harden_unix.go harden_other.go

# --- sandbox identity pool ---------------------------------------------------
# Each in-flight job is dropped to a DISTINCT uid by the service. That is what
# makes a job's 0700 working directory genuinely private: with one shared uid every
# job could read every other job's source and rewrite it before it ran.
# Runtimes that resolve getpwuid need real passwd entries, written directly here
# rather than with 32 useradd calls.
# shellcheck disable=SC2016
RUN set -eu; \
    i=0; \
    while [ "$i" -lt 32 ]; do \
      u=$((2000 + i)); \
      printf 'codhoot-s%d:x:%d:%d:codhoot sandbox:/nonexistent:/sbin/nologin\n' "$i" "$u" "$u" >> /tmp/pool; \
      i=$((i + 1)); \
    done; \
    cat /tmp/pool >> /etc/passwd; \
    rm -f /tmp/pool; \
    mkdir -p /tmp/codhoot-cache; \
    chmod 0755 /tmp/codhoot-cache

# The service itself runs as root on purpose: it must chown each job directory to
# that job's uid and setuid the compiler/runtime child. The security boundary is
# the child, not the service.
#
# There is deliberately no HEALTHCHECK here: it would require curl or wget in the
# image, and Render already health-checks /health over HTTP.
ENV PORT=8081
EXPOSE 8081
CMD ["/app/service"]