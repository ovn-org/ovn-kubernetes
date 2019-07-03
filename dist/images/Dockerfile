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

COPY kubernetes.repo /etc/yum.repos.d/kubernetes.repo
RUN yum install -y  \
	PyYAML bind-utils \
	openssl \
	numactl-libs \
	firewalld-filesystem \
	libpcap \
	hostname \
	kubectl \
	unbound-libs \
	iproute strace socat && \
	yum clean all

# Get a reasonable version of openvswitch (2.9.2 or higher)
# docker build --build-arg rpmArch=ARCH -f Dockerfile.centos -t some_tag .
# where ARCH can be x86_64 (default), aarch64, or ppc64le
ARG rpmArch=x86_64
ARG ovsVer=2.10.1
ARG ovsSubVer=3.el7
ARG dpdkVer=17.11
ARG dpdkSubVer=3.el7
ARG dpdkRpmUrl=http://cbs.centos.org/kojifiles/packages/dpdk/${dpdkVer}/${dpdkSubVer}/${rpmArch}/dpdk-${dpdkVer}-${dpdkSubVer}.${rpmArch}.rpm

RUN if [ "$(uname -m)" = "aarch64" ]; then dpdkRpmUrl=https://rpmfind.net/linux/fedora/linux/updates/28/Everything/aarch64/Packages/d/dpdk-17.11.2-1.fc28.aarch64.rpm; fi && rpm -i ${dpdkRpmUrl}
RUN rpm -i http://cbs.centos.org/kojifiles/packages/openvswitch/${ovsVer}/${ovsSubVer}/${rpmArch}/openvswitch-${ovsVer}-${ovsSubVer}.${rpmArch}.rpm
RUN rpm -i http://cbs.centos.org/kojifiles/packages/openvswitch/${ovsVer}/${ovsSubVer}/${rpmArch}/openvswitch-ovn-common-${ovsVer}-${ovsSubVer}.${rpmArch}.rpm
RUN rpm -i http://cbs.centos.org/kojifiles/packages/openvswitch/${ovsVer}/${ovsSubVer}/${rpmArch}/openvswitch-ovn-central-${ovsVer}-${ovsSubVer}.${rpmArch}.rpm
RUN rpm -i http://cbs.centos.org/kojifiles/packages/openvswitch/${ovsVer}/${ovsSubVer}/${rpmArch}/openvswitch-ovn-host-${ovsVer}-${ovsSubVer}.${rpmArch}.rpm
RUN rpm -i http://cbs.centos.org/kojifiles/packages/openvswitch/${ovsVer}/${ovsSubVer}/${rpmArch}/openvswitch-ovn-vtep-${ovsVer}-${ovsSubVer}.${rpmArch}.rpm
RUN rpm -i http://cbs.centos.org/kojifiles/packages/openvswitch/${ovsVer}/${ovsSubVer}/${rpmArch}/openvswitch-devel-${ovsVer}-${ovsSubVer}.${rpmArch}.rpm
RUN rm -rf /var/cache/yum

RUN mkdir -p /var/run/openvswitch
RUN mkdir -p /etc/cni/net.d
RUN mkdir -p /opt/cni/bin

# Built in ../../go_controller, then the binaries are copied here.
# put things where they are in the rpm
RUN mkdir -p /usr/libexec/cni/
COPY ovnkube ovn-kube-util /usr/bin/
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
