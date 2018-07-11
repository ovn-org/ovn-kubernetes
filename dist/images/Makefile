# build image for ovn overlay network cni plugin

# Both ovnkube.yaml and onvkube-master.yaml use this image
# this image is built from files in this directory and pushed to 
# a docker registry that is accesseble on each node

# The temprary (insecure) registry is netdev31:5000/ovn-kube:latest
# which is the image name in the daemonsets.
# Registry must be setup ahead of time.
# The registry is configured in /etc/containers/registries.conf
# on each node in both "registries:" and "insecure_registries:" sections.

all: bld
	docker build -t ovn-kube .
	#docker tag ovn-kube my-registry:5000/ovn-kube:latest
	#docker push my-registry:5000/ovn-kube:latest

fedora: bld
	docker build -t ovn-kube-f -f Dockerfile.fedora .
	#docker tag ovn-kube-f my-registry:5000/ovn-kube:latest
	#docker push my-registry:5000/ovn-kube:latest

.PHONY: ../../go-controller/_output/go/bin/ovnkube

../../go-controller/_output/go/bin/ovnkube:
	cd ../../go-controller ; make

bld: ../../go-controller/_output/go/bin/ovnkube
	cp ../../go-controller/_output/go/bin/* .
	mkdir -p .git/ .git/refs/heads/
	cp ../../.git/HEAD .git/HEAD
	cp ../../.git/refs/heads/* .git/refs/heads/

