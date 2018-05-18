#
# This is the OpenShift sdn-ovn image.
# it provides an overlay network using ovs/ovn/ovnkube
#
# The standard name for this image is origin-network-ovn

# Notes:
# The ovn-kubernetes utilities can come from the openvswitch-ovn-kubernetes rpm
# or built in go_controller, copied to this directory and copied to the container.
# The question becomes one of determining which version are we using.
#
# At present this uses fedora:28, this may default to centos:7 
# when the needed ovs/ovn rpms are at 2.8 or higher and the
# openvswitch-ovn-kubernetes is available.
#
# So this file will change over time.

FROM fedora:28

MAINTAINER Phil Cameron <pcameron@redhat.com>

USER root

ENV PYTHONDONTWRITEBYTECODE yes

RUN dnf install --refresh -y  \
	PyYAML bind-utils procps-ng hostname \
	openvswitch openvswitch-ovn-common openvswitch-ovn-host openvswitch-ovn-central \
	openvswitch-ovn-docker python2-openvswitch openvswitch-ovn-vtep \
	containernetworking-cni \
	jq iproute strace socat && \
	dnf clean all

RUN mkdir -p /var/run/openvswitch
RUN mkdir -p /etc/cni/net.d
RUN mkdir -p /opt/cni/bin

# Built in ../go_controller, then copied to ../image
# alternatively, install openvswitch-ovn-kubernetes rpm
COPY ovn-k8s-cni-overlay /opt/cni/bin/ovn-k8s-cni-overlay
COPY ovn-k8s-overlay ovnkube ovn-kube-util /usr/bin/

# ovnkube.sh is the entry point. This script examines environment
# variables to direct operation and configure ovn
COPY ovnkube.sh /root/
COPY ovn-debug.sh /root/
# override the rpm's ovn_k8s.conf with this local copy
COPY ovn_k8s.conf /etc/openvswitch/ovn_k8s.conf

WORKDIR /root
ENTRYPOINT /root/ovnkube.sh
