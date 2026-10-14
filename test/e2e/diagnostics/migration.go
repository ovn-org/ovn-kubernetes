// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package diagnostics

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	. "github.com/onsi/gomega"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
)

// MigrationCapture retains host captures beyond launcher and namespace deletion.
// Separate IPv4/IPv6 rings prevent healthy IPv4 traffic overwriting a stalled
// IPv6 connection. Host tcpdump does not see every OVS-internal forwarding hop.
func (d *Diagnostics) MigrationCapture(port int32) func(bool) {
	if os.Getenv("KUBEVIRT_MIGRATION_DEBUG") != "true" {
		return func(bool) {}
	}
	ns := d.fr.Namespace.Name
	dir := "/tmp/kubevirt-migration-debug/" + ns
	cmd := fmt.Sprintf(`set -eu
mkdir -p /host%[1]s
cd /host%[1]s
rm -f ready stopped
exec >collector.log 2>&1
date -u --iso-8601=ns
command -v tcpdump conntrack ovs-appctl
pids=""
finish() {
  trap - EXIT TERM INT
  for pid in $pids; do kill "$pid" 2>/dev/null || true; done
  wait || true
  touch stopped
}
trap finish EXIT TERM INT
for family in ip ip6; do
  tcpdump -Z root -U -n -i any -s 128 -C 16 -W 8 -w "$family.pcap" "$family and (tcp port 5201 or tcp port %[2]d or icmp or icmp6)" 2>"$family.stderr" &
  pids="$pids $!"
done
tcpdump -Z root -U -n -i any -s 192 -C 16 -W 4 -w geneve.pcap 'udp port 6081' 2>geneve.stderr &
pids="$pids $!"
for family in ipv4 ipv6; do
  for port in 5201 %[2]d; do
    conntrack -E -f "$family" -p tcp --dport "$port" -o timestamp,extended >"$family-$port.events" 2>&1 &
    pids="$pids $!"
  done
done
sleep 1
for pid in $pids; do kill -0 "$pid"; done
touch ready
iteration=0
while [ ! -e stop ]; do
  date -u --iso-8601=ns
  for family in ipv4 ipv6; do
    for port in 5201 %[2]d; do
      conntrack -L -f "$family" -p tcp --dport "$port" -o extended || true
    done
  done
  ovs-appctl dpctl/dump-conntrack | grep -E 'sport=(5201|%[2]d)(,|\))|dport=(5201|%[2]d)(,|\))' || true
  ip -6 route show table all
  ip -6 neigh show
  if [ "$((iteration %% 5))" = 0 ]; then
    ovn-nbctl --timeout=2 --db=unix:/host/run/ovn/ovnnb_db.sock --format=json --columns=name,vips,options list load_balancer || true
    ovn-nbctl --timeout=2 --db=unix:/host/run/ovn/ovnnb_db.sock --format=json list logical_router_static_route || true
    ovn-sbctl --timeout=2 --db=unix:/host/run/ovn/ovnsb_db.sock --format=json --columns=logical_port,chassis,up,options list port_binding || true
  fi
  iteration=$((iteration + 1))
  sleep 1
done
finish
exec sleep infinity
`, dir, port)
	ds := d.composeDiagnosticsDaemonSet("migration-capture", cmd, "migration-capture")
	Expect(d.runDaemonSets([]appsv1.DaemonSet{ds})).To(Succeed())
	Eventually(func() bool {
		current, err := d.fr.ClientSet.AppsV1().DaemonSets(ns).Get(context.Background(), ds.Name, metav1.GetOptions{})
		return err == nil && current.Status.DesiredNumberScheduled > 0 &&
			current.Status.NumberReady == current.Status.DesiredNumberScheduled
	}).WithTimeout(time.Minute).WithPolling(time.Second).Should(BeTrue())
	pods, err := d.fr.ClientSet.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{LabelSelector: "app=migration-capture"})
	Expect(err).NotTo(HaveOccurred())
	for _, pod := range pods.Items {
		Eventually(func() error {
			_, err := infraprovider.Get().ExecK8NodeCommand(pod.Spec.NodeName, []string{"test", "-f", dir + "/ready"})
			return err
		}).WithTimeout(time.Minute).WithPolling(time.Second).Should(Succeed(), "capture must start on %s", pod.Spec.NodeName)
	}
	return func(passed bool) {
		for _, pod := range pods.Items {
			// Stop and flush writers before collecting or removing packet rings.
			_, err := infraprovider.Get().ExecK8NodeCommand(pod.Spec.NodeName, []string{"bash", "-c",
				fmt.Sprintf("touch %s/stop; timeout 15 bash -c 'until test -f %s/stopped; do sleep 1; done'", dir, dir)})
			framework.Logf("migration capture stop on %s: %v", pod.Spec.NodeName, err)
			if passed && err == nil {
				_, err = infraprovider.Get().ExecK8NodeCommand(pod.Spec.NodeName, []string{"bash", "-c", "rm -f " + dir + "/*.pcap*"})
				framework.Logf("remove successful migration packet rings on %s: %v", pod.Spec.NodeName, err)
			}
		}
	}
}

// MigrationAPIState samples resource changes rather than relying on a snapshot
// taken after the failing VM has already been deleted.
func (d *Diagnostics) MigrationAPIState() func() {
	if os.Getenv("KUBEVIRT_MIGRATION_DEBUG") != "true" {
		return func() {}
	}
	dir := filepath.Join(framework.TestContext.ReportDir, "migration-debug")
	Expect(os.MkdirAll(dir, 0755)).To(Succeed())
	f, err := os.Create(filepath.Join(dir, d.fr.Namespace.Name+".jsonl"))
	Expect(err).NotTo(HaveOccurred())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	watchDone := make(chan struct{})
	watchFile, err := os.Create(filepath.Join(dir, d.fr.Namespace.Name+"-endpointslices.jsonl"))
	Expect(err).NotTo(HaveOccurred())
	go func() {
		defer close(watchDone)
		defer watchFile.Close()
		version := "0"
		path := "/apis/discovery.k8s.io/v1/namespaces/" + d.fr.Namespace.Name + "/endpointslices"
		for ctx.Err() == nil {
			stream, err := d.fr.ClientSet.CoreV1().RESTClient().Get().AbsPath(path).
				SetHeader("Accept", "application/json").Param("watch", "true").
				Param("resourceVersion", version).Param("timeoutSeconds", "30").Stream(ctx)
			if err == nil {
				decoder := json.NewDecoder(stream)
				for {
					var event struct {
						Type   string          `json:"type"`
						Object json.RawMessage `json:"object"`
					}
					if err = decoder.Decode(&event); err != nil {
						break
					}
					_ = json.NewEncoder(watchFile).Encode(map[string]interface{}{"time": time.Now().UTC(), "event": event})
					var object struct {
						Metadata metav1.ObjectMeta `json:"metadata"`
					}
					if event.Type == "ERROR" {
						version = "0"
						break
					}
					if json.Unmarshal(event.Object, &object) == nil && object.Metadata.ResourceVersion != "" {
						version = object.Metadata.ResourceVersion
					}
				}
				_ = stream.Close()
			}
			if err != nil {
				_ = json.NewEncoder(watchFile).Encode(map[string]interface{}{"time": time.Now().UTC(), "watchError": err.Error()})
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
	go func() {
		defer close(done)
		defer f.Close()
		previous := map[string]string{}
		paths := map[string]string{
			"pods":           "/api/v1/namespaces/" + d.fr.Namespace.Name + "/pods",
			"services":       "/api/v1/namespaces/" + d.fr.Namespace.Name + "/services",
			"endpointslices": "/apis/discovery.k8s.io/v1/namespaces/" + d.fr.Namespace.Name + "/endpointslices",
			"vmis":           "/apis/kubevirt.io/v1/namespaces/" + d.fr.Namespace.Name + "/virtualmachineinstances",
			"migrations":     "/apis/kubevirt.io/v1/namespaces/" + d.fr.Namespace.Name + "/virtualmachineinstancemigrations",
			"nodes":          "/api/v1/nodes",
		}
		for {
			for resource, path := range paths {
				requestCtx, stop := context.WithTimeout(ctx, 5*time.Second)
				data, err := d.fr.ClientSet.CoreV1().RESTClient().Get().AbsPath(path).DoRaw(requestCtx)
				stop()
				if err != nil {
					_ = json.NewEncoder(f).Encode(map[string]interface{}{"time": time.Now().UTC(), "resource": resource, "error": err.Error()})
				} else if previous[resource] != string(data) {
					_ = json.NewEncoder(f).Encode(map[string]interface{}{"time": time.Now().UTC(), "resource": resource, "data": json.RawMessage(data)})
					previous[resource] = string(data)
				}
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Second):
			}
		}
	}()
	return func() { cancel(); <-done; <-watchDone }
}
