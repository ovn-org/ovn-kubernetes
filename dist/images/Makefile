# build image for ovn overlay network cni plugin

# ovnkube-db.yaml, ovnkube-node.yaml, and onvkube-master.yaml use this image.
# This image is built from files in this directory and pushed to
# a docker registry that is accesseble on each node.

# For a user created registry, the registry must be setup ahead of time.
# The registry is configured in /etc/containers/registries.conf
# on each node in both "registries:" and "insecure_registries:" sections.

all: ubuntu centos fedora

ubuntu: bld
	docker build -t ovn-kube-u -f Dockerfile.ubuntu .
	# This is the default in the ovnkube*.yaml files
	# docker login -u ovnkube docker.io/ovnkube
	# docker push docker.io/ovnkube/ovn-daemonset-u:latest
	./daemonset.sh --image=docker.io/ovnkube/ovn-daemonset-u:latest

ovndb-vip: bld
	docker build -t ovn-kube-u -f Dockerfile.ubuntu .
	docker build -t ovndb-vip-u -f Dockerfile-ovndb-vip.ubuntu .
	# Note: the built yaml files from the template files is incomplete since the
	# user needs to provide the VirtualIP address for the Corosync/Pacemaker setup.
	./daemonset.sh --image=docker.io/ovnkube/ovn-daemonset-u:latest \
	--db-vip-image=docker.io/ovnkube/ovndb-vip-u:latest

centos: bld
	docker build -t ovn-daemonset .
	docker tag ovn-daemonset docker.io/ovnkube/ovn-daemonset:latest
	# docker login -u ovnkube docker.io/ovnkube
	# docker push docker.io/ovnkube/ovn-daemonset:latest
	./daemonset.sh --image=docker.io/ovnkube/ovn-daemonset:latest

fedora: bld
	docker build -t ovn-kube-f -f Dockerfile.fedora .
	# docker login -u ovnkube docker.io/ovnkube
	# docker push docker.io/ovnkube/ovn-daemonset-f:latest
	./daemonset.sh --image=docker.io/ovnkube/ovn-daemonset-f:latest

fedora-dev: bld
	# To build (OVN Datapath)  with specific kernel version, please override KERNEL_VERSION.
	# By default it will build with the host machine's kernel version.

	docker build --build-arg KERNEL_VERSION=`uname -r` -t ovn-kube-f-dev -f Dockerfile.fedora.dev .
	# docker login -u ovnkube docker.io/ovnkube
	# docker push docker.io/ovnkube/ovn-daemonset-f:latest
	./daemonset.sh --image=docker.io/ovnkube/ovn-daemonset-f:latest \
                    --net-cidr=10.244.0.0/16 \
                    --svc-cidr=10.96.0.0/12 \
                    --gateway-mode="local" \
                    --master-loglevel="5" \
                    --node-loglevel="5" \
                    --ovn-loglevel-northd="-vconsole:info -vfile:info" \
                    --ovn-loglevel-nb="-vconsole:info -vfile:info" \
                    --ovn-loglevel-sb="-vconsole:info -vfile:info" \
                    --ovn-loglevel-controller="-vconsole:info" \
                    --ovn-loglevel-nbctld="-vconsole:info"

# This target expands the daemonset yaml templates into final form
# It gets the image name from ../ansible/hosts
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
