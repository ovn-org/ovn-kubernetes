from golang:1.12

RUN apt-get update && apt-get install --no-install-recommends -y \
    bc \
    gcc-multilib \
    libssl-dev  \
    llvm-dev \
    libjemalloc-dev \
    libnuma-dev  \
    python-sphinx \
    libelf-dev \
    selinux-policy-dev \
    libunbound-dev \
    autoconf \
    automake \
    libtool

# Cache go mod dependencies to speed up test execution
WORKDIR /src
ADD go.mod /src/
ADD go.sum /src/
ENV GO111MODULE=on
RUN go get -v ./...

# Prepare the environment (builds ovsdb)
WORKDIR /src/travis
ADD .travis /src/travis
ENV OVN_SRCDIR=/src/
RUN sh ./test_prepare.sh
