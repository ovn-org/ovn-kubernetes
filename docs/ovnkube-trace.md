
# ovnkube-trace

A tool to trace packet simulations for arbitrary UDP or TCP traffic between points in an ovn-kubernetes driven cluster.

### Usage:

Given the command-line arguments, ovnkube-trace would inspect the cluster to determine the addresses (MAC and IP) of the source and destination and perform `ovn-trace`, `ovs-appctl ofproto/trace`, and `ovn-detrace` from/to both directions.

```
Usage of _output/go/bin/ovnkube-trace:
  -dst string
    	dest: destination pod name
  -dst-namespace string
    	k8s namespace of dest pod (default "default")
  -dst-port string
    	dst-port: destination port (default "80")
  -kubeconfig string
    	absolute path to the kubeconfig file
  -loglevel string
    	loglevel: klog level (default "0")
  -ovn-config-namespace string
    	namespace used by ovn-config itself
  -service string
    	service: destination service name
  -src string
    	src: source pod name
  -src-namespace string
    	k8s namespace of source pod (default "default")
  -tcp
    	use tcp transport protocol
  -udp
    	use udp transport protocol
```

Currently implemented loglevels are: 
* `0` (minimal output)
* `2` (more verbose output showing results of trace commands) 
* and `5` (debug output)

#### Example

In an environment between 2 pods in namespace `default`, where the pods are named `fedora-deployment-7575f87ff9-48dbw` and `fedora-deployment-7575f87ff9-4r5pg`, the goal would be to trace UDP traffic on port 53 between both pods.
~~~
# kubectl get pods
NAME                                 READY   STATUS    RESTARTS   AGE
fedora-deployment-7575f87ff9-48dbw   1/1     Running   0          2m58s
fedora-deployment-7575f87ff9-4r5pg   1/1     Running   0          2m58s
fedora-deployment-7575f87ff9-tn992   1/1     Running   0          2m58s
~~~

The command that one would run in this case would be:
~~~
ovnkube-trace \
  -src-namespace default \
  -src fedora-deployment-7575f87ff9-48dbw \
  -dst-namespace default \
  -dst fedora-deployment-7575f87ff9-4r5pg \
  -udp -dst-port 53 \
  -loglevel 0
~~~

The result with loglevel 0 would be for a successful trace:
~~~
# ovnkube-trace -src-namespace default -src fedora-deployment-7575f87ff9-48dbw -dst-namespace default -dst fedora-deployment-7575f87ff9-4r5pg -udp -dst-port 53 -loglevel 0
I0816 13:16:58.082249   44595 ovs.go:98] Maximum command line arguments set to: 191102
I0816 13:16:58.082404   44595 ovnkube-trace.go:520] Log level set to: 0
ovn-trace indicates success from fedora-deployment-7575f87ff9-48dbw to fedora-deployment-7575f87ff9-4r5pg - matched on output to "default_fedora-deployment-7575f87ff9-4r5pg"
I0816 13:16:59.070855   44595 ovnkube-trace.go:782] ovn-trace indicates success from fedora-deployment-7575f87ff9-48dbw to fedora-deployment-7575f87ff9-4r5pg - matched on output to "default_fedora-deployment-7575f87ff9-4r5pg"
ovn-trace indicates success from fedora-deployment-7575f87ff9-4r5pg to fedora-deployment-7575f87ff9-48dbw - matched on output to "default_fedora-deployment-7575f87ff9-48dbw"
I0816 13:16:59.174819   44595 ovnkube-trace.go:824] ovn-trace indicates success from fedora-deployment-7575f87ff9-4r5pg to fedora-deployment-7575f87ff9-48dbw - matched on output to "default_fedora-deployment-7575f87ff9-48dbw"
ovs-appctl ofproto/trace indicates success from fedora-deployment-7575f87ff9-48dbw to fedora-deployment-7575f87ff9-4r5pg - matched on -> output to kernel tunnel
I0816 13:16:59.259357   44595 ovnkube-trace.go:868] ovs-appctl ofproto/trace indicates success from fedora-deployment-7575f87ff9-48dbw to fedora-deployment-7575f87ff9-4r5pg - matched on -> output to kernel tunnel
ovs-appctl ofproto/trace indicates success from fedora-deployment-7575f87ff9-4r5pg to fedora-deployment-7575f87ff9-48dbw - matched on -> output to kernel tunnel
I0816 13:16:59.337930   44595 ovnkube-trace.go:912] ovs-appctl ofproto/trace indicates success from fedora-deployment-7575f87ff9-4r5pg to fedora-deployment-7575f87ff9-48dbw - matched on -> output to kernel tunnel
ovn-trace command Completed normally
~~~

In order to see the actual trace output of the `ovn-trace` and `ovs-appctl ofproto/trace` commands, one can increase the loglevel to `2`, and in order to see debug output for the ovnkube-trace application one can raise the loglevel to `5`:
~~~
# ovnkube-trace -src-namespace default -src fedora-deployment-7575f87ff9-48dbw -dst-namespace default -dst fedora-deployment-7575f87ff9-4r5pg -udp -dst-port 53 -loglevel 2
I0816 13:19:29.375676   48571 ovs.go:98] Maximum command line arguments set to: 191102
I0816 13:19:29.375805   48571 ovnkube-trace.go:520] Log level set to: 2
I0816 13:19:30.599918   48571 ovnkube-trace.go:771] Source to Destination ovn-trace Output: # udp,reg14=0x5,vlan_tci=0x0000,dl_src=0a:58:0a:f4:02:05,dl_dst=0a:58:0a:f4:02:01,nw_src=10.244.2.5,nw_dst=10.244.0.5,nw_tos=0,nw_ecn=0,nw_ttl=64,tp_src=52888,tp_dst=53

ingress(dp="ovn-worker", inport="default_fedora-deployment-7575f87ff9-48dbw")
-----------------------------------------------------------------------------
 0. ls_in_port_sec_l2 (ovn-northd.c:4827): inport == "default_fedora-deployment-7575f87ff9-48dbw" && eth.src == {0a:58:0a:f4:02:05}, priority 50, uuid c1492caa
    next;
 1. ls_in_port_sec_ip (ovn-northd.c:4475): inport == "default_fedora-deployment-7575f87ff9-48dbw" && eth.src == 0a:58:0a:f4:02:05 && ip4.src == {10.244.2.5}, priority 90, uuid f211082a
    next;
 5. ls_in_pre_acl (ovn-northd.c:5077): ip, priority 100, uuid 5d5e5c13
    reg0[0] = 1;
    next;
 6. ls_in_pre_lb (ovn-northd.c:5245): ip, priority 100, uuid b3ce927f
    reg0[2] = 1;
    next;
 7. ls_in_pre_stateful (ovn-northd.c:5272): reg0[2] == 1 && ip4 && udp, priority 120, uuid 75329884
    reg1 = ip4.dst;
    reg2[0..15] = udp.dst;
    ct_lb;

ct_lb
-----
 8. ls_in_acl_hint (ovn-northd.c:5363): !ct.trk, priority 5, uuid b1ae686d
    reg0[8] = 1;
    reg0[9] = 1;
    next;
22. ls_in_l2_lkup (ovn-northd.c:7471): eth.dst == 0a:58:0a:f4:02:01, priority 50, uuid 86e4a0b5
    outport = "stor-ovn-worker";
    output;

egress(dp="ovn-worker", inport="default_fedora-deployment-7575f87ff9-48dbw", outport="stor-ovn-worker")
-------------------------------------------------------------------------------------------------------
 0. ls_out_pre_lb (ovn-northd.c:4973): ip && outport == "stor-ovn-worker", priority 110, uuid b3a37dd4
    next;
 1. ls_out_pre_acl (ovn-northd.c:4973): ip && outport == "stor-ovn-worker", priority 110, uuid e0c69fa1
    next;
 3. ls_out_acl_hint (ovn-northd.c:5363): !ct.trk, priority 5, uuid ab712eb3
    reg0[8] = 1;
    reg0[9] = 1;
    next;
 9. ls_out_port_sec_l2 (ovn-northd.c:4922): outport == "stor-ovn-worker", priority 50, uuid 29361162
    output;
    /* output to "stor-ovn-worker", type "patch" */

ingress(dp="ovn_cluster_router", inport="rtos-ovn-worker")
----------------------------------------------------------
 0. lr_in_admission (ovn-northd.c:9541): eth.dst == 0a:58:0a:f4:02:01 && inport == "rtos-ovn-worker", priority 50, uuid 306c2a4c
    xreg0[0..47] = 0a:58:0a:f4:02:01;
    next;
 1. lr_in_lookup_neighbor (ovn-northd.c:9621): 1, priority 0, uuid d0306840
    reg9[2] = 1;
    next;
 2. lr_in_learn_neighbor (ovn-northd.c:9630): reg9[2] == 1, priority 100, uuid 02f6f008
    next;
10. lr_in_ip_routing (ovn-northd.c:8586): ip4.dst == 10.244.0.0/24, priority 49, uuid 9fd0a82a
    ip.ttl--;
    reg8[0..15] = 0;
    reg0 = ip4.dst;
    reg1 = 10.244.0.1;
    eth.src = 0a:58:0a:f4:00:01;
    outport = "rtos-ovn-worker2";
    flags.loopback = 1;
    next;
11. lr_in_ip_routing_ecmp (ovn-northd.c:9888): reg8[0..15] == 0, priority 150, uuid 04608732
    next;
12. lr_in_policy (ovn-northd.c:7917): ip4.src == 10.244.0.0/16 && ip4.dst == 10.244.0.0/16, priority 101, uuid e383958d
    reg8[0..15] = 0;
    next;
13. lr_in_policy_ecmp (ovn-northd.c:10015): reg8[0..15] == 0, priority 150, uuid 0559eadd
    next;
14. lr_in_arp_resolve (ovn-northd.c:10191): outport == "rtos-ovn-worker2" && reg0 == 10.244.0.5, priority 100, uuid 7c4780c7
    eth.dst = 0a:58:0a:f4:00:05;
    next;
18. lr_in_arp_request (ovn-northd.c:10639): 1, priority 0, uuid c06bf600
    output;

egress(dp="ovn_cluster_router", inport="rtos-ovn-worker", outport="rtos-ovn-worker2")
-------------------------------------------------------------------------------------
 3. lr_out_delivery (ovn-northd.c:10686): outport == "rtos-ovn-worker2", priority 100, uuid a4c152ed
    output;
    /* output to "rtos-ovn-worker2", type "patch" */

ingress(dp="ovn-worker2", inport="stor-ovn-worker2")
----------------------------------------------------
 0. ls_in_port_sec_l2 (ovn-northd.c:4827): inport == "stor-ovn-worker2", priority 50, uuid 8f724726
    next;
 5. ls_in_pre_acl (ovn-northd.c:4970): ip && inport == "stor-ovn-worker2", priority 110, uuid c5287f8c
    next;
 6. ls_in_pre_lb (ovn-northd.c:4970): ip && inport == "stor-ovn-worker2", priority 110, uuid 4f909463
    next;
 8. ls_in_acl_hint (ovn-northd.c:5363): !ct.trk, priority 5, uuid b1ae686d
    reg0[8] = 1;
    reg0[9] = 1;
    next;
22. ls_in_l2_lkup (ovn-northd.c:7471): eth.dst == 0a:58:0a:f4:00:05, priority 50, uuid c0bd3710
    outport = "default_fedora-deployment-7575f87ff9-4r5pg";
    output;

egress(dp="ovn-worker2", inport="stor-ovn-worker2", outport="default_fedora-deployment-7575f87ff9-4r5pg")
---------------------------------------------------------------------------------------------------------
 0. ls_out_pre_lb (ovn-northd.c:5247): ip, priority 100, uuid 3bb5afdb
    reg0[2] = 1;
    next;
 1. ls_out_pre_acl (ovn-northd.c:5079): ip, priority 100, uuid e0e3076b
    reg0[0] = 1;
    next;
 2. ls_out_pre_stateful (ovn-northd.c:5292): reg0[2] == 1, priority 110, uuid c6d31e9c
    ct_lb;

ct_lb
-----
 3. ls_out_acl_hint (ovn-northd.c:5363): !ct.trk, priority 5, uuid ab712eb3
    reg0[8] = 1;
    reg0[9] = 1;
    next;
 8. ls_out_port_sec_ip (ovn-northd.c:4475): outport == "default_fedora-deployment-7575f87ff9-4r5pg" && eth.dst == 0a:58:0a:f4:00:05 && ip4.dst == {255.255.255.255, 224.0.0.0/4, 10.244.0.5}, priority 90, uuid 17d3bc0b
    next;
 9. ls_out_port_sec_l2 (ovn-northd.c:4922): outport == "default_fedora-deployment-7575f87ff9-4r5pg" && eth.dst == {0a:58:0a:f4:00:05}, priority 50, uuid a1ed6b2a
    output;
    /* output to "default_fedora-deployment-7575f87ff9-4r5pg", type "" */

ovn-trace indicates success from fedora-deployment-7575f87ff9-48dbw to fedora-deployment-7575f87ff9-4r5pg - matched on output to "default_fedora-deployment-7575f87ff9-4r5pg"
I0816 13:19:30.600023   48571 ovnkube-trace.go:782] ovn-trace indicates success from fedora-deployment-7575f87ff9-48dbw to fedora-deployment-7575f87ff9-4r5pg - matched on output to "default_fedora-deployment-7575f87ff9-4r5pg"
I0816 13:19:30.697730   48571 ovnkube-trace.go:814] Destination to Source ovn-trace Output: # udp,reg14=0x5,vlan_tci=0x0000,dl_src=0a:58:0a:f4:00:05,dl_dst=0a:58:0a:f4:00:01,nw_src=10.244.0.5,nw_dst=10.244.2.5,nw_tos=0,nw_ecn=0,nw_ttl=64,tp_src=53,tp_dst=52888

ingress(dp="ovn-worker2", inport="default_fedora-deployment-7575f87ff9-4r5pg")
------------------------------------------------------------------------------
 0. ls_in_port_sec_l2 (ovn-northd.c:4827): inport == "default_fedora-deployment-7575f87ff9-4r5pg" && eth.src == {0a:58:0a:f4:00:05}, priority 50, uuid e7ac0947
    next;
 1. ls_in_port_sec_ip (ovn-northd.c:4475): inport == "default_fedora-deployment-7575f87ff9-4r5pg" && eth.src == 0a:58:0a:f4:00:05 && ip4.src == {10.244.0.5}, priority 90, uuid 9d5e1f6c
    next;
 5. ls_in_pre_acl (ovn-northd.c:5077): ip, priority 100, uuid 5d5e5c13
    reg0[0] = 1;
    next;
 6. ls_in_pre_lb (ovn-northd.c:5245): ip, priority 100, uuid b3ce927f
    reg0[2] = 1;
    next;
 7. ls_in_pre_stateful (ovn-northd.c:5272): reg0[2] == 1 && ip4 && udp, priority 120, uuid 75329884
    reg1 = ip4.dst;
    reg2[0..15] = udp.dst;
    ct_lb;

ct_lb
-----
 8. ls_in_acl_hint (ovn-northd.c:5363): !ct.trk, priority 5, uuid b1ae686d
    reg0[8] = 1;
    reg0[9] = 1;
    next;
22. ls_in_l2_lkup (ovn-northd.c:7471): eth.dst == 0a:58:0a:f4:00:01, priority 50, uuid ddfb7162
    outport = "stor-ovn-worker2";
    output;

egress(dp="ovn-worker2", inport="default_fedora-deployment-7575f87ff9-4r5pg", outport="stor-ovn-worker2")
---------------------------------------------------------------------------------------------------------
 0. ls_out_pre_lb (ovn-northd.c:4973): ip && outport == "stor-ovn-worker2", priority 110, uuid 850a080c
    next;
 1. ls_out_pre_acl (ovn-northd.c:4973): ip && outport == "stor-ovn-worker2", priority 110, uuid 16ae3e11
    next;
 3. ls_out_acl_hint (ovn-northd.c:5363): !ct.trk, priority 5, uuid ab712eb3
    reg0[8] = 1;
    reg0[9] = 1;
    next;
 9. ls_out_port_sec_l2 (ovn-northd.c:4922): outport == "stor-ovn-worker2", priority 50, uuid 63ac4fb9
    output;
    /* output to "stor-ovn-worker2", type "patch" */

ingress(dp="ovn_cluster_router", inport="rtos-ovn-worker2")
-----------------------------------------------------------
 0. lr_in_admission (ovn-northd.c:9541): eth.dst == 0a:58:0a:f4:00:01 && inport == "rtos-ovn-worker2", priority 50, uuid 458761eb
    xreg0[0..47] = 0a:58:0a:f4:00:01;
    next;
 1. lr_in_lookup_neighbor (ovn-northd.c:9621): 1, priority 0, uuid d0306840
    reg9[2] = 1;
    next;
 2. lr_in_learn_neighbor (ovn-northd.c:9630): reg9[2] == 1, priority 100, uuid 02f6f008
    next;
10. lr_in_ip_routing (ovn-northd.c:8586): ip4.dst == 10.244.2.0/24, priority 49, uuid 5df261fc
    ip.ttl--;
    reg8[0..15] = 0;
    reg0 = ip4.dst;
    reg1 = 10.244.2.1;
    eth.src = 0a:58:0a:f4:02:01;
    outport = "rtos-ovn-worker";
    flags.loopback = 1;
    next;
11. lr_in_ip_routing_ecmp (ovn-northd.c:9888): reg8[0..15] == 0, priority 150, uuid 04608732
    next;
12. lr_in_policy (ovn-northd.c:7917): ip4.src == 10.244.0.0/16 && ip4.dst == 10.244.0.0/16, priority 101, uuid e383958d
    reg8[0..15] = 0;
    next;
13. lr_in_policy_ecmp (ovn-northd.c:10015): reg8[0..15] == 0, priority 150, uuid 0559eadd
    next;
14. lr_in_arp_resolve (ovn-northd.c:10191): outport == "rtos-ovn-worker" && reg0 == 10.244.2.5, priority 100, uuid d345259c
    eth.dst = 0a:58:0a:f4:02:05;
    next;
18. lr_in_arp_request (ovn-northd.c:10639): 1, priority 0, uuid c06bf600
    output;

egress(dp="ovn_cluster_router", inport="rtos-ovn-worker2", outport="rtos-ovn-worker")
-------------------------------------------------------------------------------------
 3. lr_out_delivery (ovn-northd.c:10686): outport == "rtos-ovn-worker", priority 100, uuid 994063f5
    output;
    /* output to "rtos-ovn-worker", type "patch" */

ingress(dp="ovn-worker", inport="stor-ovn-worker")
--------------------------------------------------
 0. ls_in_port_sec_l2 (ovn-northd.c:4827): inport == "stor-ovn-worker", priority 50, uuid 1c2c92a1
    next;
 5. ls_in_pre_acl (ovn-northd.c:4970): ip && inport == "stor-ovn-worker", priority 110, uuid 1fc9fb2a
    next;
 6. ls_in_pre_lb (ovn-northd.c:4970): ip && inport == "stor-ovn-worker", priority 110, uuid d1cbff4a
    next;
 8. ls_in_acl_hint (ovn-northd.c:5363): !ct.trk, priority 5, uuid b1ae686d
    reg0[8] = 1;
    reg0[9] = 1;
    next;
22. ls_in_l2_lkup (ovn-northd.c:7471): eth.dst == 0a:58:0a:f4:02:05, priority 50, uuid 10017335
    outport = "default_fedora-deployment-7575f87ff9-48dbw";
    output;

egress(dp="ovn-worker", inport="stor-ovn-worker", outport="default_fedora-deployment-7575f87ff9-48dbw")
-------------------------------------------------------------------------------------------------------
 0. ls_out_pre_lb (ovn-northd.c:5247): ip, priority 100, uuid 3bb5afdb
    reg0[2] = 1;
    next;
 1. ls_out_pre_acl (ovn-northd.c:5079): ip, priority 100, uuid e0e3076b
    reg0[0] = 1;
    next;
 2. ls_out_pre_stateful (ovn-northd.c:5292): reg0[2] == 1, priority 110, uuid c6d31e9c
    ct_lb;

ct_lb
-----
 3. ls_out_acl_hint (ovn-northd.c:5363): !ct.trk, priority 5, uuid ab712eb3
    reg0[8] = 1;
    reg0[9] = 1;
    next;
 8. ls_out_port_sec_ip (ovn-northd.c:4475): outport == "default_fedora-deployment-7575f87ff9-48dbw" && eth.dst == 0a:58:0a:f4:02:05 && ip4.dst == {255.255.255.255, 224.0.0.0/4, 10.244.2.5}, priority 90, uuid 8cd1eae0
    next;
 9. ls_out_port_sec_l2 (ovn-northd.c:4922): outport == "default_fedora-deployment-7575f87ff9-48dbw" && eth.dst == {0a:58:0a:f4:02:05}, priority 50, uuid 3801a65e
    output;
    /* output to "default_fedora-deployment-7575f87ff9-48dbw", type "" */

ovn-trace indicates success from fedora-deployment-7575f87ff9-4r5pg to fedora-deployment-7575f87ff9-48dbw - matched on output to "default_fedora-deployment-7575f87ff9-48dbw"
I0816 13:19:30.697918   48571 ovnkube-trace.go:824] ovn-trace indicates success from fedora-deployment-7575f87ff9-4r5pg to fedora-deployment-7575f87ff9-48dbw - matched on output to "default_fedora-deployment-7575f87ff9-48dbw"
I0816 13:19:30.776016   48571 ovnkube-trace.go:851] Source to Destination ovs-appctl Output: Flow: udp,in_port=7,vlan_tci=0x0000,dl_src=0a:58:0a:f4:02:05,dl_dst=0a:58:0a:f4:02:01,nw_src=10.244.2.5,nw_dst=10.244.0.5,nw_tos=0,nw_ecn=0,nw_ttl=64,tp_src=12345,tp_dst=53
(...)
~~~
