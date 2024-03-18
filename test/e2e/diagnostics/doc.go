/*
The diagnostics package contains different tools to collect data they can be
executed using the following flags at the test suite

- `--collect-conntrack`: Call `conntrack -L`
- `--collect-iptables`: Call `iptables -L -n`
- `--collect-ovsflows`: Call `ovs-ofctl dump-flows` per interface
- `--collect-tcpdump`: Call `tcpdump -vvv -nne` per interface and expression

# To integratre them with new test first instance the diagnostics package with

```golang
fr                 = wrappedTestFramework("my-test-suite")
d                  = diagnostics.New(fr)
```

# Then at the beginning of test initialize conntrack, ovsflows and iptables like

```golang

	d.ConntrackDumpingDaemonSet()
	d.OVSFlowsDumpingDaemonSet("breth0")
	d.IPTablesDumpingDaemonSet()

```

And add tcpdump to the test place where the expression can be composed, for example to dump tcpdump related to a service nodeport, the expression will be composed after creating the service, also specifying the interfaces.

```golang

	d.TCPDumpDaemonSet([]string{"any", "eth0", "breth0"}, fmt.Sprintf("port %s or port %s", port, nodePort))

```
*/
package diagnostics
