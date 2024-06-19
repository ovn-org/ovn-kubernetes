# build image for ovn overlay network cni plugin

# ovnkube-db.yaml, ovnkube-node.yaml, and onvkube-master.yaml use this image.
# This image is built from files in this directory and pushed to
# a docker registry that is accesseble on each node.

# For a user created registry, the registry must be setup ahead of time.
# The registry is configured in /etc/containers/registries.conf
# on each node in both "registries:" and "insecure_registries:" sections.

all: ubuntu fedora

SLASH = -
ARCH = $(subst aarch64,arm64,$(subst x86_64,amd64,$(patsubst i%86,386,$(shell uname -m))))
IMAGE_ARCH = $(SLASH)$(ARCH)
DOCKERFILE_ARCH =
ifeq ($(ARCH),arm64)
        DOCKERFILE_ARCH=.arm64
endif
OVS_BRANCH ?= master
OVN_BRANCH ?= main
OCI_BIN ?= docker

# The image of ovnkube/ovn-daemonset-ubuntu should be multi-arched before using it on arm64
ubuntu: bld
	${OCI_BIN} build -t ovn-kube-ubuntu$(IMAGE_ARCH) -f Dockerfile.ubuntu$(DOCKERFILE_ARCH) .
ifeq ($(ARCH),amd64)
	${OCI_BIN} tag "ovn-kube-ubuntu$(IMAGE_ARCH):latest" \
              "ovn-kube-ubuntu:latest"
endif
	# This is the default in the ovnkube*.yaml files
	# ${OCI_BIN} login -u ovnkube docker.io/ovnkube
	# ${OCI_BIN} push docker.io/ovnkube/ovn-daemonset-ubuntu:latest
	./daemonset.sh --image=docker.io/ovnkube/ovn-daemonset-ubuntu:latest

fedora: bld
	${OCI_BIN} build -t ovn-kube-fedora -f Dockerfile.fedora .
	# ${OCI_BIN} login -u ovnkube docker.io/ovnkube
	# ${OCI_BIN} push docker.io/ovnkube/ovn-daemonset-fedora:latest
	./daemonset.sh --image=docker.io/ovnkube/ovn-daemonset-fedora:latest

fedora-dev: bld
	${OCI_BIN} build \
		     --build-arg OVS_BRANCH=$(OVS_BRANCH) \
		     --build-arg OVN_BRANCH=$(OVN_BRANCH) \
		     -t ovn-kube-fedora-dev -f Dockerfile.fedora.dev .
	# ${OCI_BIN} login -u ovnkube docker.io/ovnkube
	# ${OCI_BIN} push docker.io/ovnkube/ovn-daemonset-fedora:latest
	./daemonset.sh --image=docker.io/ovnkube/ovn-daemonset-fedora:latest \
                    --net-cidr=10.244.0.0/16 \
                    --svc-cidr=10.96.0.0/12 \
                    --gateway-mode="local" \
                    --master-loglevel="5" \
                    --node-loglevel="5" \
                    --ovn-loglevel-northd="-vconsole:info -vfile:info" \
                    --ovn-loglevel-nb="-vconsole:info -vfile:info" \
                    --ovn-loglevel-sb="-vconsole:info -vfile:info" \
                    --ovn-loglevel-controller="-vconsole:info" \
                    --ovn_nb_raft_election_timer="1000" \
                    --ovn_sb_raft_election_timer="1000"

DOCKER_IMAGE_TAG = latest

# Multi-arch the ubuntu based image with fat-manifest
ubuntu-image-multi-arch:
	./push_manifest.sh ovn-daemonset-ubuntu $(DOCKER_IMAGE_TAG)

# This target expands the daemonset yaml templates into final form
# Use CLI flags or environment variables to customize its behavior.
daemonsetyaml:
	./daemonset.sh

.PHONY: ../../go-controller/_output/go/bin/ovnkube

../../go-controller/_output/go/bin/ovnkube:
	cd ../../go-controller ; make

BRANCH = $(shell git rev-parse  --symbolic-full-name HEAD)
COMMIT = $(shell git rev-parse  HEAD)
bld: ../../go-controller/_output/go/bin/ovnkube
	cp -r ../../go-controller/_output/go/bin/* .
	echo "ref: ${BRANCH}  commit: ${COMMIT}" > git_info
