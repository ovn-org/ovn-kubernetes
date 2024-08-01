#!/bin/bash -xe

for node in $(kind get nodes --name ovn); do
    oc exec -it pod-$node -- ping -c1 100.65.0.2
    oc exec -it pod-$node -- ping -c1 100.65.0.3
    oc exec -it pod-$node -- ping -c1 100.65.0.4
done

nginx_address=$(oc get pod  nginx -o json |jq -r '.metadata.annotations["k8s.ovn.org/pod-networks"]' |jq -r '."default/access-tenant-blue".ip_address'|sed "s#/16##")

for node in $(kind get nodes --name ovn); do
    ./contrib/exec.sh $node ovnkube-node ovnkube-controller ovn-nbctl lb-del LB
    ./contrib/exec.sh $node ovnkube-node ovnkube-controller ovn-nbctl lb-add LB 172.20.0.3:666 ${nginx_address}:80 tcp
    ./contrib/exec.sh $node ovnkube-node ovnkube-controller ovn-nbctl ls-lb-add tenantblue_ovn_layer2_switch LB
    ./contrib/exec.sh $node ovnkube-node ovnkube-controller ovn-nbctl lr-lb-add GR_tenantblue_$node LB
done
