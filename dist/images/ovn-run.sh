#!/bin/bash

# run the ovs-vswitchd daemon from a container

#docker run --rm --network host --mount type=bind,src=/var/run/openvswitch,dst=/var/run/openvswitch  phil-ovn &
#docker run --rm --network host -v /var/run/openvswitch:/var/run/openvswitch:rw  phil-ovn
#docker run --rm --network host -v /var/run/openvswitch/db.sock:/var/run/openvswitch/db.sock:ro  phil-ovn
#docker run --rm --network host -v /var/run/openvswitch/db.sock:/var/run/openvswitch/db.sock:ro -v /etc/origin/node/client-ca.crt:/etc/origin/node/client-ca.crt:ro phil-ovn
docker run --rm --network host \
	-e OVN_MASTER="true" \
	-v /var/run/openvswitch/db.sock:/var/run/openvswitch/db.sock:ro \
	-v /etc/origin/node/client-ca.crt:/etc/origin/node/client-ca.crt:ro \
	-v /var/run/openvswitch/br-int.mgmt:/var/run/openvswitch/br-int.mgmt:ro \
	-v /var/run/openvswitch/br-int.snoop:/var/run/openvswitch/br-int.snoop:ro \
	-v /var/run/openvswitch/br0.mgmt:/var/run/openvswitch/br0.mgmt:ro \
	-v /var/run/openvswitch/br0.snoop:/var/run/openvswitch/br0.snoop:ro \
	-v /opt/cni/bin:/host/opt/cni/bin:rw \
	-v /etc/cni/net.d:/etc/cni/net.d:rw \
	-v /var/lib/cni/networks/ovn-k8s-cni-overlay:/var/lib/cni/networks/ovn-k8s-cni-overlay:rw \
	netdev22:5000/phil-ovn &

exit 0
