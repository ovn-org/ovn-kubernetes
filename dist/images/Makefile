# build image for ovn cni

# both ovnkube.yaml and onvkube-master.yaml use this image
# this image is built from files in this directory and pushed to 
# a docker repository that is accesseble on each node

# The temprary (insecure) repository is netdev22:5000/ovn-kube:latest

all:
	docker build -t ovn-kube .
	docker tag ovn-kube netdev22:5000/ovn-kube:latest
	docker push netdev22:5000/ovn-kube:latest

