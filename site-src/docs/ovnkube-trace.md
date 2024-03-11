
# ovnkube-trace

A tool to trace packet simulations for arbitrary UDP or TCP traffic between points in an ovn-kubernetes driven cluster.

### Usage:

Given the command-line arguments, ovnkube-trace would inspect the cluster to determine the addresses (MAC and IP) of the source and destination and perform `ovn-trace`, `ovs-appctl ofproto/trace`, and `ovn-detrace` from/to both directions.

```
Usage of _output/go/bin/ovnkube-trace:
  -addr-family string
    	Address family (ip4 or ip6) to be used for tracing (default "ip4")
  -dst string
    	dest: destination pod name
  -dst-ip string
    	destination IP address (meant for tests to external targets)
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
  -skip-detrace
    	skip ovn-detrace command
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

In an environment between 2 pods in namespace `default`, where the pods are named `fedora-deployment-7d49fddf69-chmvh` and `fedora-deployment-7d49fddf69-t4hqw`, the goal would be to trace UDP traffic on port 53 between both pods. Each node in the cluster is running in a different interconnect zone.
~~~
# kubectl get pods -o wide
NAME                                 READY   STATUS    RESTARTS   AGE   IP           NODE                NOMINATED NODE   READINESS GATES
fedora-deployment-7d49fddf69-chmvh   1/1     Running   0          10m   10.244.2.3   ovn-worker2         <none>           <none>
fedora-deployment-7d49fddf69-t4hqw   1/1     Running   0          10m   10.244.1.6   ovn-worker          <none>           <none>
fedora-deployment-7d49fddf69-vwjt7   1/1     Running   0          10m   10.244.0.3   ovn-control-plane   <none>           <none>
~~~

The command that one would run in this case would be:
~~~
ovnkube-trace \
  -src-namespace default \
  -src fedora-deployment-7d49fddf69-chmvh \
  -dst-namespace default \
  -dst fedora-deployment-7d49fddf69-t4hqw \
  -udp -dst-port 53 \
  -loglevel 0
~~~

The result with loglevel 0 would be for a successful trace:
~~~
# ovnkube-trace -src-namespace default -src fedora-deployment-7d49fddf69-chmvh -dst-namespace default -dst fedora-deployment-7d49fddf69-t4hqw -udp -dst-port 53 -loglevel 0
ovn-trace source pod to destination pod indicates success from fedora-deployment-7d49fddf69-chmvh to fedora-deployment-7d49fddf69-t4hqw
ovn-trace (remote) source pod to destination pod indicates success from fedora-deployment-7d49fddf69-chmvh to fedora-deployment-7d49fddf69-t4hqw
ovn-trace destination pod to source pod indicates success from fedora-deployment-7d49fddf69-t4hqw to fedora-deployment-7d49fddf69-chmvh
ovn-trace (remote) destination pod to source pod indicates success from fedora-deployment-7d49fddf69-t4hqw to fedora-deployment-7d49fddf69-chmvh
ovs-appctl ofproto/trace source pod to destination pod indicates success from fedora-deployment-7d49fddf69-chmvh to fedora-deployment-7d49fddf69-t4hqw
ovs-appctl ofproto/trace destination pod to source pod indicates success from fedora-deployment-7d49fddf69-t4hqw to fedora-deployment-7d49fddf69-chmvh
ovn-detrace source pod to destination pod indicates success from fedora-deployment-7d49fddf69-chmvh to fedora-deployment-7d49fddf69-t4hqw
ovn-detrace destination pod to source pod indicates success from fedora-deployment-7d49fddf69-t4hqw to fedora-deployment-7d49fddf69-chmvh
~~~

In order to see the actual trace output of the `ovn-trace` and `ovs-appctl ofproto/trace` commands, one can increase the loglevel to `2`, and in order to see debug output for the ovnkube-trace application one can raise the loglevel to `5`:
~~~
# ovnkube-trace -src-namespace default -src fedora-deployment-7d49fddf69-chmvh -dst-namespace default -dst fedora-deployment-7d49fddf69-t4hqw -udp -dst-port 53 -loglevel 2
I0823 21:33:18.112821 2457963 ovnkube-trace.go:1157] Log level set to: 2
I0823 21:33:18.857705 2457963 ovnkube-trace.go:693] ovn-trace source pod to destination pod Output:
# udp,reg14=0x3,vlan_tci=0x0000,dl_src=0a:58:0a:f4:02:03,dl_dst=0a:58:0a:f4:02:01,nw_src=10.244.2.3,nw_dst=10.244.1.6,nw_tos=0,nw_ecn=0,nw_ttl=64,nw_frag=no,tp_src=52888,tp_dst=53

ingress(dp="ovn-worker2", inport="default_fedora-deployment-7d49fddf69-chmvh")
------------------------------------------------------------------------------
 0. ls_in_check_port_sec (northd.c:8583): 1, priority 50, uuid de664d3a
    reg0[15] = check_in_port_sec();
    next;
 4. ls_in_pre_acl (northd.c:5991): ip, priority 100, uuid d9a60156
    reg0[0] = 1;
    next;
 5. ls_in_pre_lb (northd.c:6178): ip, priority 100, uuid 4f6825b1
    reg0[2] = 1;
    next;
 6. ls_in_pre_stateful (northd.c:6201): reg0[2] == 1, priority 110, uuid 82c039a6
    ct_lb_mark;

ct_lb_mark /* default (use --ct to customize) */
------------------------------------------------
 7. ls_in_acl_hint (northd.c:6297): !ct.new && ct.est && !ct.rpl && ct_mark.blocked == 0, priority 4, uuid 0b20013d
    reg0[8] = 1;
    reg0[10] = 1;
    next;
 9. ls_in_acl_action (northd.c:6764): reg8[30..31] == 0, priority 500, uuid 3eec76bb
    reg8[30..31] = 1;
    next(8);
 9. ls_in_acl_action (northd.c:6764): reg8[30..31] == 1, priority 500, uuid b327f7af
    reg8[30..31] = 2;
    next(8);
 9. ls_in_acl_action (northd.c:6753): 1, priority 0, uuid b0f710dc
    reg8[16] = 0;
    reg8[17] = 0;
    reg8[18] = 0;
    reg8[30..31] = 0;
    next;
15. ls_in_pre_hairpin (northd.c:7786): ip && ct.trk, priority 100, uuid 88ef5011
    reg0[6] = chk_lb_hairpin();
    reg0[12] = chk_lb_hairpin_reply();
    next;
19. ls_in_acl_after_lb_action (northd.c:6764): reg8[30..31] == 0, priority 500, uuid 01461328
    reg8[30..31] = 1;
    next(18);
19. ls_in_acl_after_lb_action (northd.c:6764): reg8[30..31] == 1, priority 500, uuid a22021af
    reg8[30..31] = 2;
    next(18);
19. ls_in_acl_after_lb_action (northd.c:6753): 1, priority 0, uuid 2c98ee3d
    reg8[16] = 0;
    reg8[17] = 0;
    reg8[18] = 0;
    reg8[30..31] = 0;
    next;
27. ls_in_l2_lkup (northd.c:9407): eth.dst == { 0a:58:a9:fe:01:01, 0a:58:0a:f4:02:01 }, priority 50, uuid b29511a2
    outport = "stor-ovn-worker2";
    output;

egress(dp="ovn-worker2", inport="default_fedora-deployment-7d49fddf69-chmvh", outport="stor-ovn-worker2")
---------------------------------------------------------------------------------------------------------
 0. ls_out_pre_acl (northd.c:5878): ip && outport == "stor-ovn-worker2", priority 110, uuid 8b1bef96
    next;
 1. ls_out_pre_lb (northd.c:5878): ip && outport == "stor-ovn-worker2", priority 110, uuid d7c53e71
    next;
 3. ls_out_acl_hint (northd.c:6297): !ct.new && ct.est && !ct.rpl && ct_mark.blocked == 0, priority 4, uuid 26b08bb5
    reg0[8] = 1;
    reg0[10] = 1;
    next;
 5. ls_out_acl_action (northd.c:6764): reg8[30..31] == 0, priority 500, uuid ea58bb8e
    reg8[30..31] = 1;
    next(4);
 5. ls_out_acl_action (northd.c:6764): reg8[30..31] == 1, priority 500, uuid 03897328
    reg8[30..31] = 2;
    next(4);
 5. ls_out_acl_action (northd.c:6753): 1, priority 0, uuid bcfbe611
    reg8[16] = 0;
    reg8[17] = 0;
    reg8[18] = 0;
    reg8[30..31] = 0;
    next;
 9. ls_out_check_port_sec (northd.c:5843): 1, priority 0, uuid 79358872
    reg0[15] = check_out_port_sec();
    next;
10. ls_out_apply_port_sec (northd.c:5848): 1, priority 0, uuid 12ba0dbe
    output;
    /* output to "stor-ovn-worker2", type "patch" */

ingress(dp="ovn_cluster_router", inport="rtos-ovn-worker2")
-----------------------------------------------------------
 0. lr_in_admission (northd.c:11790): eth.dst == { 0a:58:a9:fe:01:01, 0a:58:0a:f4:02:01 } && inport == "rtos-ovn-worker2" && is_chassis_resident("cr-rtos-ovn-worker2"), priority 50, uuid e40942af
    xreg0[0..47] = 0a:58:0a:f4:02:01;
    next;
 1. lr_in_lookup_neighbor (northd.c:11956): 1, priority 0, uuid 897d00f0
    reg9[2] = 1;
    next;
 2. lr_in_learn_neighbor (northd.c:11965): reg9[2] == 1 || reg9[3] == 0, priority 100, uuid 234da6ee
    next;
12. lr_in_ip_routing_pre (northd.c:12190): 1, priority 0, uuid b1a3a6af
    reg7 = 0;
    next;
13. lr_in_ip_routing (northd.c:10603): reg7 == 0 && ip4.dst == 10.244.1.0/24, priority 73, uuid 1ed4e720
    ip.ttl--;
    reg8[0..15] = 0;
    reg0 = 100.88.0.4;
    reg1 = 100.88.0.2;
    eth.src = 0a:58:a8:fe:00:02;
    outport = "rtots-ovn-worker2";
    flags.loopback = 1;
    next;
14. lr_in_ip_routing_ecmp (northd.c:12285): reg8[0..15] == 0, priority 150, uuid a1ea724a
    next;
15. lr_in_policy (northd.c:9741): ip4.src == 10.244.0.0/16 && ip4.dst == 10.244.0.0/16, priority 102, uuid 1c6af09a
    reg8[0..15] = 0;
    next;
16. lr_in_policy_ecmp (northd.c:12452): reg8[0..15] == 0, priority 150, uuid 3841a2fc
    next;
17. lr_in_arp_resolve (northd.c:12665): outport == "rtots-ovn-worker2" && reg0 == 100.88.0.4, priority 100, uuid 792c14a1
    eth.dst = 0a:58:a8:fe:00:04;
    next;
21. lr_in_arp_request (northd.c:13083): 1, priority 0, uuid f21b210a
    output;

egress(dp="ovn_cluster_router", inport="rtos-ovn-worker2", outport="rtots-ovn-worker2")
---------------------------------------------------------------------------------------
 0. lr_out_chk_dnat_local (northd.c:14444): 1, priority 0, uuid 2f6e84ed
    reg9[4] = 0;
    next;
 6. lr_out_delivery (northd.c:13129): outport == "rtots-ovn-worker2", priority 100, uuid 81cdee53
    output;
    /* output to "rtots-ovn-worker2", type "patch" */

ingress(dp="transit_switch", inport="tstor-ovn-worker2")
--------------------------------------------------------
 0. ls_in_check_port_sec (northd.c:8583): 1, priority 50, uuid de664d3a
    reg0[15] = check_in_port_sec();
    next;
 5. ls_in_pre_lb (northd.c:5875): ip && inport == "tstor-ovn-worker2", priority 110, uuid 69169a39
    next;
27. ls_in_l2_lkup (northd.c:9329): eth.dst == 0a:58:a8:fe:00:04, priority 50, uuid da101703
    outport = "tstor-ovn-worker";
    output;

egress(dp="transit_switch", inport="tstor-ovn-worker2", outport="tstor-ovn-worker")
-----------------------------------------------------------------------------------
 9. ls_out_check_port_sec (northd.c:5843): 1, priority 0, uuid 79358872
    reg0[15] = check_out_port_sec();
    next;
10. ls_out_apply_port_sec (northd.c:5848): 1, priority 0, uuid 12ba0dbe
    output;
    /* output to "tstor-ovn-worker", type "remote" */

ovn-trace source pod to destination pod indicates success from fedora-deployment-7d49fddf69-chmvh to fedora-deployment-7d49fddf69-t4hqw
I0823 21:33:18.858296 2457963 ovnkube-trace.go:704] Search string matched:
output to "tstor-ovn-worker"
(...)
I0823 21:33:19.169751 2457963 ovnkube-trace.go:693] ovs-appctl ofproto/trace source pod to destination pod Output:
Flow: udp,in_port=7,vlan_tci=0x0000,dl_src=0a:58:0a:f4:02:03,dl_dst=0a:58:0a:f4:02:01,nw_src=10.244.2.3,nw_dst=10.244.1.6,nw_tos=0,nw_ecn=0,nw_ttl=64,nw_frag=no,tp_src=12345,tp_dst=53

bridge("br-int")
----------------
 0. in_port=7, priority 100, cookie 0x6c1d0b4a
    set_field:0x13->reg13
    set_field:0xb->reg11
    set_field:0x6->reg12
    set_field:0x3->metadata
    set_field:0x3->reg14
    resubmit(,8)
 8. metadata=0x3, priority 50, cookie 0xde664d3a
    set_field:0/0x1000->reg10
    resubmit(,73)
    73. ip,reg14=0x3,metadata=0x3,dl_src=0a:58:0a:f4:02:03,nw_src=10.244.2.3, priority 90, cookie 0x6c1d0b4a
            set_field:0/0x1000->reg10
    move:NXM_NX_REG10[12]->NXM_NX_XXREG0[111]
     -> NXM_NX_XXREG0[111] is now 0
    resubmit(,9)
 9. metadata=0x3, priority 0, cookie 0x57b52622
    resubmit(,10)
10. metadata=0x3, priority 0, cookie 0x98964ef0
    resubmit(,11)
11. metadata=0x3, priority 0, cookie 0x72c60524
    resubmit(,12)
12. ip,metadata=0x3, priority 100, cookie 0xd9a60156
    set_field:0x1000000000000000000000000/0x1000000000000000000000000->xxreg0
    resubmit(,13)
13. ip,metadata=0x3, priority 100, cookie 0x4f6825b1
    set_field:0x4000000000000000000000000/0x4000000000000000000000000->xxreg0
    resubmit(,14)
14. ip,reg0=0x4/0x4,metadata=0x3, priority 110, cookie 0x82c039a6
    ct(table=15,zone=NXM_NX_REG13[0..15],nat)
    nat
     -> A clone of the packet is forked to recirculate. The forked pipeline will be resumed at table 15.
     -> Sets the packet to an untracked state, and clears all the conntrack fields.

Final flow: udp,reg0=0x5,reg11=0xb,reg12=0x6,reg13=0x13,reg14=0x3,metadata=0x3,in_port=7,vlan_tci=0x0000,dl_src=0a:58:0a:f4:02:03,dl_dst=0a:58:0a:f4:02:01,nw_src=10.244.2.3,nw_dst=10.244.1.6,nw_tos=0,nw_ecn=0,nw_ttl=64,nw_frag=no,tp_src=12345,tp_dst=53
Megaflow: recirc_id=0,eth,udp,in_port=7,dl_src=0a:58:0a:f4:02:03,dl_dst=0a:58:0a:f4:02:01,nw_src=10.244.2.3,nw_dst=10.128.0.0/9,nw_frag=no,tp_src=0x2000/0xe000
Datapath actions: ct(zone=19,nat),recirc(0x12)

===============================================================================
recirc(0x12) - resume conntrack with default ct_state=trk|new (use --ct-next to customize)
Replacing src/dst IP/ports to simulate NAT:
 Initial flow: 
 Modified flow: 
===============================================================================

Flow: recirc_id=0x12,ct_state=new|trk,ct_zone=19,eth,udp,reg0=0x5,reg11=0xb,reg12=0x6,reg13=0x13,reg14=0x3,metadata=0x3,in_port=7,vlan_tci=0x0000,dl_src=0a:58:0a:f4:02:03,dl_dst=0a:58:0a:f4:02:01,nw_src=10.244.2.3,nw_dst=10.244.1.6,nw_tos=0,nw_ecn=0,nw_ttl=64,nw_frag=no,tp_src=12345,tp_dst=53

bridge("br-int")
----------------
    thaw
        Resuming from table 15
15. ct_state=+new-est+trk,metadata=0x3, priority 7, cookie 0xf14d8019
    set_field:0x80000000000000000000000000/0x80000000000000000000000000->xxreg0
    set_field:0x200000000000000000000000000/0x200000000000000000000000000->xxreg0
    resubmit(,16)
16. ct_state=-est+trk,ip,metadata=0x3, priority 1, cookie 0x8dfd1f3e
    set_field:0x2000000000000000000000000/0x2000000000000000000000000->xxreg0
    resubmit(,17)
17. reg8=0/0xc0000000,metadata=0x3, priority 500, cookie 0x3eec76bb
    set_field:0x4000000000000000/0xc000000000000000->xreg4
    resubmit(,16)
16. ct_state=-est+trk,ip,metadata=0x3, priority 1, cookie 0x8dfd1f3e
    set_field:0x2000000000000000000000000/0x2000000000000000000000000->xxreg0
    resubmit(,17)
17. reg8=0x40000000/0xc0000000,metadata=0x3, priority 500, cookie 0xb327f7af
    set_field:0x8000000000000000/0xc000000000000000->xreg4
    resubmit(,16)
16. ct_state=-est+trk,ip,metadata=0x3, priority 1, cookie 0x8dfd1f3e
    set_field:0x2000000000000000000000000/0x2000000000000000000000000->xxreg0
    resubmit(,17)
17. metadata=0x3, priority 0, cookie 0xb0f710dc
    set_field:0/0x1000000000000->xreg4
    set_field:0/0x2000000000000->xreg4
    set_field:0/0x4000000000000->xreg4
    set_field:0/0xc000000000000000->xreg4
    resubmit(,18)
18. metadata=0x3, priority 0, cookie 0xcae3d5b1
    resubmit(,19)
19. metadata=0x3, priority 0, cookie 0x66860608
    resubmit(,20)
20. metadata=0x3, priority 0, cookie 0x9db4b75e
    resubmit(,21)
21. metadata=0x3, priority 0, cookie 0x414a15a0
    resubmit(,22)
22. metadata=0x3, priority 0, cookie 0xebbc94cf
    resubmit(,23)
23. ct_state=+trk,ip,metadata=0x3, priority 100, cookie 0x88ef5011
    set_field:0/0x80->reg10
    resubmit(,68)
    68. No match.
            drop
    move:NXM_NX_REG10[7]->NXM_NX_XXREG0[102]
     -> NXM_NX_XXREG0[102] is now 0
    set_field:0/0x80->reg10
    resubmit(,69)
    69. No match.
            drop
    move:NXM_NX_REG10[7]->NXM_NX_XXREG0[108]
     -> NXM_NX_XXREG0[108] is now 0
    resubmit(,24)
24. metadata=0x3, priority 0, cookie 0x7fd98606
    resubmit(,25)
25. metadata=0x3, priority 0, cookie 0xd3d8976
    resubmit(,26)
26. metadata=0x3, priority 0, cookie 0x1ecb6f2e
    resubmit(,27)
27. reg8=0/0xc0000000,metadata=0x3, priority 500, cookie 0x1461328
    set_field:0x4000000000000000/0xc000000000000000->xreg4
    resubmit(,26)
26. metadata=0x3, priority 0, cookie 0x1ecb6f2e
    resubmit(,27)
27. reg8=0x40000000/0xc0000000,metadata=0x3, priority 500, cookie 0xa22021af
    set_field:0x8000000000000000/0xc000000000000000->xreg4
    resubmit(,26)
26. metadata=0x3, priority 0, cookie 0x1ecb6f2e
    resubmit(,27)
27. metadata=0x3, priority 0, cookie 0x2c98ee3d
    set_field:0/0x1000000000000->xreg4
    set_field:0/0x2000000000000->xreg4
    set_field:0/0x4000000000000->xreg4
    set_field:0/0xc000000000000000->xreg4
    resubmit(,28)
28. ip,reg0=0x2/0x2002,metadata=0x3, priority 100, cookie 0xd8583947
    ct(commit,zone=NXM_NX_REG13[0..15],nat(src),exec(set_field:0/0x1->ct_mark))
    nat(src)
    set_field:0/0x1->ct_mark
     -> Sets the packet to an untracked state, and clears all the conntrack fields.
    resubmit(,29)
29. metadata=0x3, priority 0, cookie 0x41080b67
    resubmit(,30)
30. metadata=0x3, priority 0, cookie 0x936e8520
    resubmit(,31)
31. metadata=0x3, priority 0, cookie 0x6d369d0e
    resubmit(,32)
32. metadata=0x3, priority 0, cookie 0x119a6138
    resubmit(,33)
33. metadata=0x3, priority 0, cookie 0x1d30f590
    resubmit(,34)
34. metadata=0x3, priority 0, cookie 0x74ef3d9
    resubmit(,35)
35. metadata=0x3,dl_dst=0a:58:0a:f4:02:01, priority 50, cookie 0xb29511a2
    set_field:0x1->reg15
    resubmit(,37)
37. priority 0
    resubmit(,38)
38. priority 0
    resubmit(,40)
40. priority 0
    resubmit(,41)
41. reg15=0x1,metadata=0x3, priority 100, cookie 0x7c79d0e2
    set_field:0xb->reg11
    set_field:0x6->reg12
    resubmit(,42)
42. priority 0
    set_field:0->reg0
    set_field:0->reg1
    set_field:0->reg2
    set_field:0->reg3
    set_field:0->reg4
    set_field:0->reg5
    set_field:0->reg6
    set_field:0->reg7
    set_field:0->reg8
    set_field:0->reg9
    resubmit(,43)
43. ip,reg15=0x1,metadata=0x3, priority 110, cookie 0x8b1bef96
    resubmit(,44)
44. ip,reg15=0x1,metadata=0x3, priority 110, cookie 0xd7c53e71
    resubmit(,45)
45. metadata=0x3, priority 0, cookie 0xe59d971f
    resubmit(,46)
46. ct_state=-trk,metadata=0x3, priority 5, cookie 0xd4c65410
    set_field:0x100000000000000000000000000/0x100000000000000000000000000->xxreg0
    set_field:0x200000000000000000000000000/0x200000000000000000000000000->xxreg0
    resubmit(,47)
47. metadata=0x3, priority 0, cookie 0x17ae0ddf
    resubmit(,48)
48. reg8=0/0xc0000000,metadata=0x3, priority 500, cookie 0xea58bb8e
    set_field:0x4000000000000000/0xc000000000000000->xreg4
    resubmit(,47)
47. metadata=0x3, priority 0, cookie 0x17ae0ddf
    resubmit(,48)
48. reg8=0x40000000/0xc0000000,metadata=0x3, priority 500, cookie 0x3897328
    set_field:0x8000000000000000/0xc000000000000000->xreg4
    resubmit(,47)
47. metadata=0x3, priority 0, cookie 0x17ae0ddf
    resubmit(,48)
48. metadata=0x3, priority 0, cookie 0xbcfbe611
    set_field:0/0x1000000000000->xreg4
    set_field:0/0x2000000000000->xreg4
    set_field:0/0x4000000000000->xreg4
    set_field:0/0xc000000000000000->xreg4
    resubmit(,49)
49. metadata=0x3, priority 0, cookie 0x39bbcf9
    resubmit(,50)
50. metadata=0x3, priority 0, cookie 0xaf9ffaea
    resubmit(,51)
51. metadata=0x3, priority 0, cookie 0xbee9000e
    resubmit(,52)
52. metadata=0x3, priority 0, cookie 0x79358872
    set_field:0/0x1000->reg10
    resubmit(,75)
    75. No match.
            drop
    move:NXM_NX_REG10[12]->NXM_NX_XXREG0[111]
     -> NXM_NX_XXREG0[111] is now 0
    resubmit(,53)
53. metadata=0x3, priority 0, cookie 0x12ba0dbe
    resubmit(,64)
64. priority 0
    resubmit(,65)
65. reg15=0x1,metadata=0x3, priority 100, cookie 0x7c79d0e2
    clone(ct_clear,set_field:0->reg11,set_field:0->reg12,set_field:0->reg13,set_field:0x8->reg11,set_field:0xe->reg12,set_field:0x1->metadata,set_field:0x2->reg14,set_field:0->reg10,set_field:0->reg15,set_field:0->reg0,set_field:0->reg1,set_field:0->reg2,set_field:0->reg3,set_field:0->reg4,set_field:0->reg5,set_field:0->reg6,set_field:0->reg7,set_field:0->reg8,set_field:0->reg9,resubmit(,8))
    ct_clear
    set_field:0->reg11
    set_field:0->reg12
    set_field:0->reg13
    set_field:0x8->reg11
    set_field:0xe->reg12
    set_field:0x1->metadata
    set_field:0x2->reg14
    set_field:0->reg10
    set_field:0->reg15
    set_field:0->reg0
    set_field:0->reg1
    set_field:0->reg2
    set_field:0->reg3
    set_field:0->reg4
    set_field:0->reg5
    set_field:0->reg6
    set_field:0->reg7
    set_field:0->reg8
    set_field:0->reg9
    resubmit(,8)
 8. reg14=0x2,metadata=0x1,dl_dst=0a:58:0a:f4:02:01, priority 50, cookie 0xe40942af
    set_field:0xa580af402010000000000000000/0xffffffffffff0000000000000000->xxreg0
    resubmit(,9)
 9. metadata=0x1, priority 0, cookie 0x897d00f0
    set_field:0x4/0x4->xreg4
    resubmit(,10)
10. reg9=0/0x8,metadata=0x1, priority 100, cookie 0x234da6ee
    resubmit(,11)
11. metadata=0x1, priority 0, cookie 0x7f28e0ff
    resubmit(,12)
12. metadata=0x1, priority 0, cookie 0x1ee2fb50
    resubmit(,13)
13. metadata=0x1, priority 0, cookie 0x17a7cfa8
    resubmit(,14)
14. metadata=0x1, priority 0, cookie 0x56f6cf85
    resubmit(,15)
15. metadata=0x1, priority 0, cookie 0x600f694f
    resubmit(,16)
16. metadata=0x1, priority 0, cookie 0xda085702
    resubmit(,17)
17. metadata=0x1, priority 0, cookie 0x30578698
    resubmit(,18)
18. metadata=0x1, priority 0, cookie 0x58ec3bea
    resubmit(,19)
19. metadata=0x1, priority 0, cookie 0xe6096bd7
    resubmit(,20)
20. metadata=0x1, priority 0, cookie 0xb1a3a6af
    set_field:0/0xffffffff->xxreg1
    resubmit(,21)
21. ip,reg7=0,metadata=0x1,nw_dst=10.244.1.0/24, priority 73, cookie 0x1ed4e720
    dec_ttl()
    set_field:0/0xffff00000000->xreg4
    set_field:0xa8fe0004000000000000000000000000/0xffffffff000000000000000000000000->xxreg0
    set_field:0xa8fe00020000000000000000/0xffffffff0000000000000000->xxreg0
    set_field:0a:58:a8:fe:00:02->eth_src
    set_field:0x4->reg15
    set_field:0x1/0x1->reg10
    resubmit(,22)
22. reg8=0/0xffff,metadata=0x1, priority 150, cookie 0xa1ea724a
    resubmit(,23)
23. ip,metadata=0x1,nw_src=10.244.0.0/16,nw_dst=10.244.0.0/16, priority 102, cookie 0x1c6af09a
    set_field:0/0xffff00000000->xreg4
    resubmit(,24)
24. reg8=0/0xffff,metadata=0x1, priority 150, cookie 0x3841a2fc
    resubmit(,25)
25. reg0=0xa8fe0004,reg15=0x4,metadata=0x1, priority 100, cookie 0x792c14a1
    set_field:0a:58:a8:fe:00:04->eth_dst
    resubmit(,26)
26. metadata=0x1, priority 0, cookie 0x8be057a3
    resubmit(,27)
27. metadata=0x1, priority 0, cookie 0x5ea30a7d
    resubmit(,28)
28. metadata=0x1, priority 0, cookie 0x79529c6a
    resubmit(,29)
29. metadata=0x1, priority 0, cookie 0xf21b210a
    resubmit(,37)
37. priority 0
    resubmit(,38)
38. priority 0
    resubmit(,40)
40. priority 0
    resubmit(,41)
41. reg15=0x4,metadata=0x1, priority 100, cookie 0x93ec5715
    set_field:0x8->reg11
    set_field:0xe->reg12
    resubmit(,42)
42. priority 0
    set_field:0->reg0
    set_field:0->reg1
    set_field:0->reg2
    set_field:0->reg3
    set_field:0->reg4
    set_field:0->reg5
    set_field:0->reg6
    set_field:0->reg7
    set_field:0->reg8
    set_field:0->reg9
    resubmit(,43)
43. metadata=0x1, priority 0, cookie 0x2f6e84ed
    set_field:0/0x10->xreg4
    resubmit(,44)
44. metadata=0x1, priority 0, cookie 0x90d7e25c
    resubmit(,45)
45. metadata=0x1, priority 0, cookie 0xe36440a6
    resubmit(,46)
46. metadata=0x1, priority 0, cookie 0xd3dbde6f
    resubmit(,47)
47. metadata=0x1, priority 0, cookie 0x519ebaf8
    resubmit(,48)
48. metadata=0x1, priority 0, cookie 0xc8fb3dc1
    resubmit(,49)
49. reg15=0x4,metadata=0x1, priority 100, cookie 0x81cdee53
    resubmit(,64)
64. reg10=0x1/0x1,reg15=0x4,metadata=0x1, priority 100, cookie 0x93ec5715
    push:NXM_OF_IN_PORT[]
    set_field:ANY->in_port
    resubmit(,65)
    65. reg15=0x4,metadata=0x1, priority 100, cookie 0x93ec5715
            clone(ct_clear,set_field:0->reg11,set_field:0->reg12,set_field:0->reg13,set_field:0x3->reg11,set_field:0x11->reg12,set_field:0xff0003->metadata,set_field:0x2->reg14,set_field:0->reg10,set_field:0->reg15,set_field:0->reg0,set_field:0->reg1,set_field:0->reg2,set_field:0->reg3,set_field:0->reg4,set_field:0->reg5,set_field:0->reg6,set_field:0->reg7,set_field:0->reg8,set_field:0->reg9,resubmit(,8))
            ct_clear
            set_field:0->reg11
            set_field:0->reg12
            set_field:0->reg13
            set_field:0x3->reg11
            set_field:0x11->reg12
            set_field:0xff0003->metadata
            set_field:0x2->reg14
            set_field:0->reg10
            set_field:0->reg15
            set_field:0->reg0
            set_field:0->reg1
            set_field:0->reg2
            set_field:0->reg3
            set_field:0->reg4
            set_field:0->reg5
            set_field:0->reg6
            set_field:0->reg7
            set_field:0->reg8
            set_field:0->reg9
            resubmit(,8)
         8. metadata=0xff0003, priority 50, cookie 0xde664d3a
            set_field:0/0x1000->reg10
            resubmit(,73)
            73. No match.
                    drop
            move:NXM_NX_REG10[12]->NXM_NX_XXREG0[111]
             -> NXM_NX_XXREG0[111] is now 0
            resubmit(,9)
         9. metadata=0xff0003, priority 0, cookie 0x57b52622
            resubmit(,10)
        10. metadata=0xff0003, priority 0, cookie 0x98964ef0
            resubmit(,11)
        11. metadata=0xff0003, priority 0, cookie 0x72c60524
            resubmit(,12)
        12. metadata=0xff0003, priority 0, cookie 0x351dd7a3
            resubmit(,13)
        13. ip,reg14=0x2,metadata=0xff0003, priority 110, cookie 0x69169a39
            resubmit(,14)
        14. metadata=0xff0003, priority 0, cookie 0x5c78cf83
            resubmit(,15)
        15. metadata=0xff0003, priority 65535, cookie 0x8ac9010
            resubmit(,16)
        16. metadata=0xff0003, priority 65535, cookie 0x973723d7
            resubmit(,17)
        17. metadata=0xff0003, priority 0, cookie 0xc722ae8c
            resubmit(,18)
        18. metadata=0xff0003, priority 0, cookie 0xcae3d5b1
            resubmit(,19)
        19. metadata=0xff0003, priority 0, cookie 0x66860608
            resubmit(,20)
        20. metadata=0xff0003, priority 0, cookie 0x9db4b75e
            resubmit(,21)
        21. metadata=0xff0003, priority 0, cookie 0x414a15a0
            resubmit(,22)
        22. metadata=0xff0003, priority 0, cookie 0xebbc94cf
            resubmit(,23)
        23. metadata=0xff0003, priority 0, cookie 0x74d6cf40
            resubmit(,24)
        24. metadata=0xff0003, priority 0, cookie 0x7fd98606
            resubmit(,25)
        25. metadata=0xff0003, priority 0, cookie 0xd3d8976
            resubmit(,26)
        26. metadata=0xff0003, priority 0, cookie 0x1ecb6f2e
            resubmit(,27)
        27. metadata=0xff0003, priority 0, cookie 0xba824d52
            resubmit(,28)
        28. metadata=0xff0003, priority 0, cookie 0xa5e04afc
            resubmit(,29)
        29. metadata=0xff0003, priority 0, cookie 0x41080b67
            resubmit(,30)
        30. metadata=0xff0003, priority 0, cookie 0x936e8520
            resubmit(,31)
        31. metadata=0xff0003, priority 0, cookie 0x6d369d0e
            resubmit(,32)
        32. metadata=0xff0003, priority 0, cookie 0x119a6138
            resubmit(,33)
        33. metadata=0xff0003, priority 0, cookie 0x1d30f590
            resubmit(,34)
        34. metadata=0xff0003, priority 0, cookie 0x74ef3d9
            resubmit(,35)
        35. metadata=0xff0003,dl_dst=0a:58:a8:fe:00:04, priority 50, cookie 0xda101703
            set_field:0x4->reg15
            resubmit(,37)
        37. priority 0
            resubmit(,38)
        38. priority 0
            resubmit(,40)
        40. reg15=0x4,metadata=0xff0003, priority 100, cookie 0xb6badb74
            set_field:0xff0003/0xffffff->tun_id
            set_field:0x4->tun_metadata0
            move:NXM_NX_REG14[0..14]->NXM_NX_TUN_METADATA0[16..30]
             -> NXM_NX_TUN_METADATA0[16..30] is now 0x2
            output:4
             -> output to kernel tunnel
            resubmit(,41)
        41. priority 0
            drop
    pop:NXM_OF_IN_PORT[]
     -> NXM_OF_IN_PORT[] is now 7

Final flow: recirc_id=0x12,eth,udp,reg0=0x300,reg11=0xb,reg12=0x6,reg13=0x13,reg14=0x3,reg15=0x1,metadata=0x3,in_port=7,vlan_tci=0x0000,dl_src=0a:58:0a:f4:02:03,dl_dst=0a:58:0a:f4:02:01,nw_src=10.244.2.3,nw_dst=10.244.1.6,nw_tos=0,nw_ecn=0,nw_ttl=64,nw_frag=no,tp_src=12345,tp_dst=53
Megaflow: recirc_id=0x12,ct_state=+new-est-rel-rpl-inv+trk,ct_mark=0/0xf,eth,udp,in_port=7,dl_src=0a:58:0a:f4:02:03,dl_dst=0a:58:0a:f4:02:01,nw_src=10.244.2.3,nw_dst=10.244.1.0/24,nw_ecn=0,nw_ttl=64,nw_frag=no
Datapath actions: ct(commit,zone=19,mark=0/0x1,nat(src)),set(tunnel(tun_id=0xff0003,dst=172.18.0.2,ttl=64,tp_dst=6081,geneve({class=0x102,type=0x80,len=4,0x20004}),flags(df|csum|key))),set(eth(src=0a:58:a8:fe:00:02,dst=0a:58:a8:fe:00:04)),set(ipv4(ttl=63)),5

ovs-appctl ofproto/trace source pod to destination pod indicates success from fedora-deployment-7d49fddf69-chmvh to fedora-deployment-7d49fddf69-t4hqw
I0823 21:33:19.170205 2457963 ovnkube-trace.go:704] Search string matched:
-> output to kernel tunnel
(...)
~~~
