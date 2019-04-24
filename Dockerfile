#
# This is the OpenShift ovn overlay network image.
# it provides an overlay network using ovs/ovn/ovn-kube
#
# The standard name for this image is ovn-kube

# Notes:
# This is for a build where the ovn-kubernetes utilities
# are built in this Dockerfile and included in the image (instead of the rpm)
#

FROM openshift/origin-release:golang-1.10 AS builder

WORKDIR /go-controller
COPY go-controller/ .

# build the binaries
RUN make

FROM openshift/origin-cli AS cli

FROM openshift/origin-base

USER root

ENV PYTHONDONTWRITEBYTECODE yes

# install needed rpms - openvswitch must be 2.9.2 or higher
# install selinux-policy first to avoid a race
RUN yum install -y  \
	selinux-policy && \
	yum clean all

RUN yum install -y  \
	PyYAML bind-utils \
	openssl \
	numactl-libs \
	firewalld-filesystem \
	libpcap \
	hostname \
	"openvswitch2.11" \
	"openvswitch2.11-ovn-common" \
	"openvswitch2.11-ovn-central" \
	"openvswitch2.11-ovn-host" \
	"openvswitch2.11-ovn-vtep" \
	"openvswitch2.11-devel" \
	containernetworking-plugins \
	iproute strace socat && \
	yum clean all

RUN rm -rf /var/cache/yum

RUN mkdir -p /var/run/openvswitch && \
    mkdir -p /etc/cni/net.d && \
    mkdir -p /opt/cni/bin && \
    mkdir -p /usr/libexec/cni/

COPY --from=builder /go-controller/_output/go/bin/ovnkube /usr/bin/
COPY --from=builder /go-controller/_output/go/bin/ovn-kube-util /usr/bin/
COPY --from=builder /go-controller/_output/go/bin/ovn-k8s-cni-overlay /usr/libexec/cni/ovn-k8s-cni-overlay

COPY --from=cli /usr/bin/oc /usr/bin
RUN ln -s /usr/bin/oc /usr/bin/kubectl

# copy git commit number into image
COPY .git/HEAD /root/.git/HEAD
COPY .git/refs/heads/ /root/.git/refs/heads/

# ovnkube.sh is the entry point. This script examines environment
# variables to direct operation and configure ovn
COPY dist/images/ovnkube.sh /root/
COPY dist/images/ovn-debug.sh /root/
# override the rpm's ovn_k8s.conf with this local copy
COPY dist/images/ovn_k8s.conf /etc/openvswitch/ovn_k8s.conf


LABEL io.k8s.display-name="ovn kubernetes" \
      io.k8s.description="This is a component of OpenShift Container Platform that provides an overlay network using ovn." \
      summary="This is a component of OpenShift Container Platform that provides an overlay network using ovn." \
      io.openshift.tags="openshift" \
      maintainer="Phil Cameron <pcameron@redhat.com>"

WORKDIR /root
ENTRYPOINT /root/ovnkube.sh

