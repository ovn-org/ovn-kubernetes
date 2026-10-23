// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package cni

import (
	"context"
	"fmt"
	"net"
	"os"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/ovn-kubernetes/libovsdb/client"

	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb"
	ovsops "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/libovsdb/ops"
	libovsdbtest "github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/testing/libovsdb"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/types"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/util"
	"github.com/ovn-kubernetes/ovn-kubernetes/go-controller/pkg/vswitchd"
)

// BenchmarkOVSClientStartup measures the per-invocation connection and initial
// monitor cost, not a complete CNI ADD/DEL. Set OVSDB_TEST_BIN_DIR to use an
// isolated ovsdb-server: the in-memory server ignores initial monitor filters.
func BenchmarkOVSClientStartup(b *testing.B) {
	if os.Getenv("OVSDB_TEST_BIN_DIR") == "" {
		b.Skip("set OVSDB_TEST_BIN_DIR to benchmark real monitor filtering")
	}
	for _, ports := range []int{10, 100, 250, 500, 1000} {
		b.Run(fmt.Sprintf("ports=%d", ports), func(b *testing.B) {
			data := shimOVSBenchmarkData(ports)
			serverClient, _ := newShimOVSTestServer(b, data)
			endpoint := serverClient.CurrentEndpoint()
			serverClient.Close()

			for _, profile := range []struct {
				name string
				opts []client.MonitorOption
			}{
				{name: "full"},
				{name: "shim-add", opts: shimOVSMonitorOptions(CNIAdd)},
				{name: "shim-del", opts: shimOVSMonitorOptions(CNIDel)},
			} {
				b.Run(profile.name, func(b *testing.B) {
					for _, parallel := range []bool{false, true} {
						b.Run(fmt.Sprintf("parallel=%t", parallel), func(b *testing.B) {
							latencies := make([]time.Duration, b.N)
							var next atomic.Int64
							run := func() {
								i := int(next.Add(1) - 1)
								stopCh := make(chan struct{})
								start := time.Now()
								c, err := libovsdb.NewOVSClientWithEndpoint(endpoint, stopCh, profile.opts...)
								latencies[i] = time.Since(start)
								if err != nil {
									close(stopCh)
									b.Error(err)
									return
								}
								if got := c.Cache().Table(vswitchd.InterfaceTable).Len(); got != ports {
									b.Errorf("initial cache has %d interfaces, want %d", got, ports)
								}
								c.Close()
								close(stopCh)
							}
							b.ResetTimer()
							b.ReportAllocs()
							if parallel {
								b.RunParallel(func(pb *testing.PB) {
									for pb.Next() {
										run()
									}
								})
							} else {
								for i := 0; i < b.N; i++ {
									run()
								}
							}
							b.StopTimer()
							sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
							b.ReportMetric(float64(latencies[len(latencies)/2])/float64(time.Millisecond), "p50-ms/startup")
							b.ReportMetric(float64(latencies[(len(latencies)-1)*95/100])/float64(time.Millisecond), "p95-ms/startup")
						})
					}
				})
			}
		})
	}
}

// BenchmarkShimOVSOperations includes client startup, ConfigureOVS and the OVS
// part of DEL. It does not measure netns setup, daemon RPC or real OVN readiness:
// readiness is emulated on an isolated database.
func BenchmarkShimOVSOperations(b *testing.B) {
	if os.Getenv("OVSDB_TEST_BIN_DIR") == "" {
		b.Skip("set OVSDB_TEST_BIN_DIR to benchmark against an isolated OVSDB server")
	}
	for _, ports := range []int{100, 500, 1000} {
		b.Run(fmt.Sprintf("ports=%d", ports), func(b *testing.B) {
			server, _ := newShimOVSTestServer(b, shimOVSBenchmarkData(ports))
			emulateShimOVSReadiness(b, server)
			for _, attachments := range []int{1, 2} {
				b.Run(fmt.Sprintf("attachments=%d", attachments), func(b *testing.B) {
					addTimes, delTimes := make([]time.Duration, b.N), make([]time.Duration, b.N)
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						start := time.Now()
						c, err := libovsdb.NewOVSClientWithEndpoint(server.CurrentEndpoint(), ctx.Done(), shimOVSMonitorOptions(CNIAdd)...)
						require.NoError(b, err)
						for n := 0; n < attachments; n++ {
							info := &PodInterfaceInfo{PodUID: "bench-uid", NetName: types.DefaultNetworkName, NADKey: types.DefaultNetworkName,
								PodAnnotation: util.PodAnnotation{MAC: net.HardwareAddr{0x0a, 0x58, 0x0a, 0x80, 0, 1}}}
							if n == 1 {
								info.NetName, info.NADKey = "blue", "bench/blue"
							}
							require.NoError(b, ConfigureOVS(ctx, c, "bench", "pod", fmt.Sprintf("eth%d", n),
								fmt.Sprintf("bench-port-%d", n), info, "bench-sandbox", "", false, nil))
						}
						c.Close()
						cancel()
						addTimes[i] = time.Since(start)

						start = time.Now()
						ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
						c, err = libovsdb.NewOVSClientWithEndpoint(server.CurrentEndpoint(), ctx.Done(), shimOVSMonitorOptions(CNIDel)...)
						require.NoError(b, err)
						names, err := findInterfacesWithSandbox(c, "bench-sandbox")
						require.NoError(b, err)
						require.Len(b, names, attachments)
						for _, name := range names {
							err = ovsops.DeletePortWithInterfaces(c, "br-int", name)
							require.NoError(b, err)
						}
						require.NoError(b, clearPodBandwidth(c, "bench-sandbox"))
						c.Close()
						cancel()
						delTimes[i] = time.Since(start)
					}
					b.StopTimer()
					for name, samples := range map[string][]time.Duration{"ADD": addTimes, "DEL": delTimes} {
						sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
						b.ReportMetric(float64(samples[len(samples)/2])/float64(time.Millisecond), "p50-ms/"+name)
						b.ReportMetric(float64(samples[(len(samples)-1)*95/100])/float64(time.Millisecond), "p95-ms/"+name)
					}
				})
			}
		})
	}
}

func shimOVSBenchmarkData(ports int) []libovsdbtest.TestData {
	bridge := &vswitchd.Bridge{UUID: "br_int", Name: "br-int"}
	data := []libovsdbtest.TestData{
		&vswitchd.OpenvSwitch{UUID: "ovs", Bridges: []string{bridge.UUID}},
		bridge,
	}
	for i := 0; i < ports; i++ {
		name := fmt.Sprintf("pod%012d", i)
		portID, ifaceID := fmt.Sprintf("port%d", i), fmt.Sprintf("iface%d", i)
		ofport, mtu := i+1, 1400
		bridge.Ports = append(bridge.Ports, portID)
		data = append(data,
			&vswitchd.Port{UUID: portID, Name: name, Interfaces: []string{ifaceID},
				OtherConfig: map[string]string{"transient": "true"}},
			&vswitchd.Interface{UUID: ifaceID, Name: name, Ofport: &ofport, MTU: &mtu,
				Statistics: map[string]int{
					"rx_bytes": 1_000_000, "rx_packets": 10_000, "tx_bytes": 2_000_000, "tx_packets": 20_000,
					"rx_dropped": 0, "tx_dropped": 0, "rx_errors": 0, "tx_errors": 0,
					"rx_crc_err": 0, "rx_frame_err": 0, "rx_over_err": 0, "collisions": 0,
				}, ExternalIDs: map[string]string{
					"iface-id":      fmt.Sprintf("namespace_pod%d", i),
					"iface-id-ver":  fmt.Sprintf("pod-uid-%d", i),
					"sandbox":       fmt.Sprintf("sandbox-%d", i),
					"attached_mac":  "0a:58:0a:80:00:01",
					"ip_addresses":  "10.128.0.1/23",
					"ovn-installed": "true",
				}},
		)
	}
	return data
}
