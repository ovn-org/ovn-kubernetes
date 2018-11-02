# build image for ovn overlay network cni plugin

# Both ovnkube.yaml and onvkube-master.yaml use this image.
# This image is built from files in this directory and pushed to
# a docker registry that is accesseble on each node.

# For a user created registry, the registry must be setup ahead of time.
# The registry is configured in /etc/containers/registries.conf
# on each node in both "registries:" and "insecure_registries:" sections.

all: bld daemonsetyaml
	docker build -t ovn-daemonset .
	# This is the default in the ovnkube*.yaml files
	docker tag ovn-daemonset docker.io/ovnkube/ovn-daemonset:latest
	# docker login -u ovnkube docker.io/ovnkube
	# docker push docker.io/ovnkube/ovn-daemonset:latest

fedora: bld daemonsetyaml
	docker build -t ovn-kube-f -f Dockerfile.fedora .
	#docker tag ovn-kube-f my-registry:5000/ovn-kube:latest
	#docker push my-registry:5000/ovn-kube:latest

ubuntu: bld daemonsetyaml
	docker build -t ovn-kube-u -f Dockerfile.ubuntu .

# This target expands the daemonset yaml templates into final form
# It gets the image name from ../ansible/hosts
daemonsetyaml:
	./daemonset.sh

.PHONY: ../../go-controller/_output/go/bin/ovnkube

../../go-controller/_output/go/bin/ovnkube:
	cd ../../go-controller ; make

bld: ../../go-controller/_output/go/bin/ovnkube
	cp ../../go-controller/_output/go/bin/* .
	mkdir -p .git/ .git/refs/heads/
	cp ../../.git/HEAD .git/HEAD
	cp ../../.git/refs/heads/* .git/refs/heads/

