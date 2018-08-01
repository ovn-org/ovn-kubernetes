#
# This is the OpenShift ovn overlay network image.
# it provides an overlay network using ovs/ovn/ovn-kube
#
# The standard name for this image is ovn-kube

# Notes:
# This is for a development build where the ovn-kubernetes utilities
# are built in this Dockerfile and included in the image (instead of the rpm)
#
# This is based on centos:7
# openvswitch rpms are from
# http://cbs.centos.org/kojifiles/packages/openvswitch/2.9.0/4.el7/x86_64/
#
# So this file will change over time.

FROM centos:7

USER root

ENV PYTHONDONTWRITEBYTECODE yes

RUN yum install -y  \
	PyYAML bind-utils \
	openssl \
	numactl-libs \
	firewalld-filesystem \
	libpcap \
	hostname \
	iproute strace socat && \
	yum clean all

# get a reasonable version of openvswitch
RUN rpm -i http://cbs.centos.org/kojifiles/packages/openvswitch/2.9.0/4.el7/x86_64/openvswitch-2.9.0-4.el7.x86_64.rpm
RUN rpm -i http://cbs.centos.org/kojifiles/packages/openvswitch/2.9.0/4.el7/x86_64/openvswitch-ovn-common-2.9.0-4.el7.x86_64.rpm
RUN rpm -i http://cbs.centos.org/kojifiles/packages/openvswitch/2.9.0/4.el7/x86_64/openvswitch-ovn-central-2.9.0-4.el7.x86_64.rpm
RUN rpm -i http://cbs.centos.org/kojifiles/packages/openvswitch/2.9.0/4.el7/x86_64/openvswitch-ovn-host-2.9.0-4.el7.x86_64.rpm
RUN rpm -i http://cbs.centos.org/kojifiles/packages/openvswitch/2.9.0/4.el7/x86_64/openvswitch-ovn-vtep-2.9.0-4.el7.x86_64.rpm
RUN rpm -i http://cbs.centos.org/kojifiles/packages/openvswitch/2.9.0/4.el7/x86_64/openvswitch-devel-2.9.0-4.el7.x86_64.rpm
RUN rpm -i http://dl.fedoraproject.org/pub/epel/7/x86_64/Packages/c/containernetworking-cni-0.5.1-1.el7.x86_64.rpm
RUN rm -rf /var/cache/yum

RUN mkdir -p /var/run/openvswitch
RUN mkdir -p /etc/cni/net.d
RUN mkdir -p /opt/cni/bin

# Built in ../../go_controller, then the binaries are copied here.
# put things where they are in the rpm
RUN mkdir -p /usr/libexec/cni/
COPY ovn-k8s-overlay ovnkube ovn-kube-util /usr/bin/
COPY ovn-k8s-cni-overlay /usr/libexec/cni/ovn-k8s-cni-overlay

# ovnkube.sh is the entry point. This script examines environment
# variables to direct operation and configure ovn
COPY ovnkube.sh /root/
COPY ovn-debug.sh /root/
# override the rpm's ovn_k8s.conf with this local copy
COPY ovn_k8s.conf /etc/openvswitch/ovn_k8s.conf

# copy git commit number into image
RUN mkdir -p /root/.git/ /root/.git/refs/heads/
COPY .git/HEAD /root/.git/HEAD
COPY .git/refs/heads/ /root/.git/refs/heads/


LABEL io.k8s.display-name="ovn kubernetes" \
      io.k8s.description="This is a component of OpenShift Container Platform that provides an overlay network using ovn." \
      io.openshift.tags="openshift" \
      maintainer="Phil Cameron <pcameron@redhat.com>"

WORKDIR /root
ENTRYPOINT /root/ovnkube.sh
