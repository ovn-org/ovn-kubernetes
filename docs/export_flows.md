# Flow Exporting

OVN-Kubernetes can export flows to a collector whose IP and Port must be reachable by each
`ovnkube-node` instance.

[The three supported flow formats are *Netflow*, *sFlow* and *IPFIX*](https://www.openvswitch.org/support/dist-docs/ovs-vswitchd.conf.db.5.html).

## Configuring the Flow collectors IP and ports

Depending on the flow format you are willing to use, you can specify a comma-separated list of
`IP:port` pairs (or just `:port`, as explained below) in one of the following forms:

* The `ipfix-targets`, `sflow-targets` or `netflow-targets` command-line arguments of
  the `ovnkube` command.

* The `OVN_NETFLOW_TARGETS`, `OVN_SFLOW_TARGETS`, or `OVN_IPFIX_TARGETS` environment variables, if
  you run `ovnkube` as a virtual image following our provided instructions for e.g. [KIND](./kind.md),
  [Kubeadm](./INSTALL.KUBEADM.md) or [Openshift](INSTALL.OPENSHIFT.md).

* In the `/etc/openvswitch/ovn_k8s.conf` [configuration file](./config.md), under the `[monitoring]`
  section, the `ipfix-targets`, `sflow-targets` or `netflow-targets` properties.

If any of the above collector endpoints entries are expressed only as a colon followed by a port
number omiting the IP address (e.g. `:8880`), `ovnkube` will send the flows to its own Kubernetes'
Node IP, assuming that the provided port exists as a Node Port.

## Tuning IPFIX parameters

For IPFIX export, you can also specify some configuration variables to trade off between the
accuracy and the resources consumed by the flow export feature:

* **Sampling**. Fraction of packets that are sampled and sent to each target collector. This means that,
  given an `s` value for this property, 1 packet out of every `s` will be sent.
  * Default value: 400 (1 out of 400 packets).
  * Can be specified as:
      - `-ifs` or `--ipfix-sampling` CLI arguments.
      - `sampling` property in the `[ipfix]` section of the [ovn_k8s.conf configuration file](./config.md).
      - `OVN_IPFIX_SAMPLING` environment variable.

* **Cache Max Flows**. Maximum number of IPFIX flow records that can be cached at a time. If 0, caching
  is disabled.
  * Default value: 0 (disabled).
  * Can be specified as:
    - `-ifm` or `--ipfix-cache-max-flows` CLI arguments.
    - `cache-max-flows` property in the `[ipfix]` section of the [ovn_k8s.conf configuration file](./config.md).
    - `OVN_IPFIX_CACHE_MAX_FLOWS` environment variable.

* **Cache Active Timeout**. Maximum period in seconds for which an IPFIX flow record is cached and
  aggregated before being sent. If 0, caching is disabled.
  * Default value: 60.
    - The OVN-Kubernetes default behavior overrides the actual OpenVSwitch default, which is 0.
  * Can be specified as:
    - `-ifa` or `--ipfix-cache-active-timeout` CLI arguments.
    - `cache-active-timeout` property in the `[ipfix]` section of the [ovn_k8s.conf configuration file](./config.md).
    - `OVN_IPFIX_CACHE_ACTIVE_TIMEOUT` environment variable.

## More info

* [Request for Enhancement: Openshift Flow export configuration](https://github.com/openshift/enhancements/blob/master/enhancements/network/ovs-flow-export-configuration.md)

