#
# This is the OpenShift ovn overlay network image.
# it provides an overlay network using ovs/ovn/ovn-kube
#
# The standard name for this image is ovn-kube

FROM registry.ci.openshift.org/ocp/builder:rhel-9-golang-1.19-openshift-4.13 AS builder

WORKDIR /go/src/github.com/openshift/ovn-kubernetes
COPY . .

# build the binaries
RUN cd go-controller; CGO_ENABLED=0 make
RUN cd go-controller; CGO_ENABLED=0 make windows

# ovn-kubernetes-base image is built from Dockerfile.base
# The following changes are included in ovn-kubernetes-base
# image and removed from this Dockerfile:
# - ovs base rpm package installation (including openvswitch and python3-openvswitch)
# - ovn base rpm package installation (including ovn, ovn-central and ovn-host)
# - creating directories required by ovn-kubernetes
# - git commit number
# - ovnkube.sh script
FROM registry.ci.openshift.org/ocp/4.13:ovn-kubernetes-base-rhel-9

USER root

ENV PYTHONDONTWRITEBYTECODE yes

# more-pkgs file is updated in Dockerfile.base
# more-pkgs file contains the following ovs/ovn packages to be installed in this Dockerfile
# - openvswitch-devel
# - openvswitch-ipsec
# - ovn-vtep
RUN INSTALL_PKGS=" \
	openssl firewalld-filesystem \
	libpcap iproute iproute-tc strace \
	containernetworking-plugins \
	tcpdump iputils \
	libreswan \
	ethtool conntrack-tools \
	openshift-clients \
	" && \
	dnf install -y --nodocs $INSTALL_PKGS && \
	eval "dnf install -y --nodocs $(cat /more-pkgs)" && \
	dnf clean all && rm -rf /var/cache/*

COPY --from=builder /go/src/github.com/openshift/ovn-kubernetes/go-controller/_output/go/bin/ovnkube /usr/bin/
COPY --from=builder /go/src/github.com/openshift/ovn-kubernetes/go-controller/_output/go/bin/ovn-kube-util /usr/bin/
COPY --from=builder /go/src/github.com/openshift/ovn-kubernetes/go-controller/_output/go/bin/ovn-k8s-cni-overlay /usr/libexec/cni/
COPY --from=builder /go/src/github.com/openshift/ovn-kubernetes/go-controller/_output/go/bin/windows/hybrid-overlay-node.exe /root/windows/
COPY --from=builder /go/src/github.com/openshift/ovn-kubernetes/go-controller/_output/go/bin/ovndbchecker /usr/bin/
COPY --from=builder /go/src/github.com/openshift/ovn-kubernetes/go-controller/_output/go/bin/ovnkube-trace /usr/bin/

RUN stat /usr/bin/oc

LABEL io.k8s.display-name="ovn kubernetes" \
      io.k8s.description="This is a component of OpenShift Container Platform that provides an overlay network using ovn." \
      summary="This is a component of OpenShift Container Platform that provides an overlay network using ovn." \
      io.openshift.tags="openshift" \
      maintainer="Tim Rozet <trozet@redhat.com>"

WORKDIR /root
ENTRYPOINT /root/ovnkube.sh

