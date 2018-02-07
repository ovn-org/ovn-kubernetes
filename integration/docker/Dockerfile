FROM vmware/photon:2.0

MAINTAINER Bo Gan <ganb@vmware.com>

USER root

ENV PYTHONDONTWRITEBYTECODE yes

RUN tdnf install --refresh -y -q PyYAML bindutils openvswitch ovn-common ovn-host ovn-central git jq iproute2 strace && tdnf clean all

ADD ovn-go.tar.gz /opt/ovn-go-kube/

COPY entrypoint.bash install-cni.bash kubeapi.bash common-api.bash init-node.bash watcher.bash ovsdb.bash ovs-vswitchd.bash ovn-controller.bash ovs-common.inc ovnnb-db.bash ovnsb-db.bash ovn-northd.bash ovn-central-common.inc /root/

WORKDIR /root
ENTRYPOINT /root/entrypoint.bash
