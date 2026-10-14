// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"os"
	"path/filepath"
	"time"

	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider"
	infraapi "github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/infraprovider/api"
	"github.com/ovn-kubernetes/ovn-kubernetes/test/e2e/kubevirt"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/kubernetes/test/e2e/framework"
	kubevirtv1 "kubevirt.io/api/core/v1"
)

func dumpMigrationClientState(container infraapi.ExternalContainer, namespace, name string) {
	if os.Getenv("KUBEVIRT_MIGRATION_DEBUG") != "true" {
		return
	}
	output, err := infraprovider.Get().ExecExternalContainerCommand(container, []string{"bash", "-c",
		"date -u --iso-8601=ns; ss -tin; ip -6 route; ip -6 neigh; cat /tmp/ingress_test_*_iperf3.log"})
	framework.Logf("Migration client state %s/%s: %v", namespace, name, err)
	dir := filepath.Join(framework.TestContext.ReportDir, "migration-debug")
	if err := os.MkdirAll(dir, 0755); err != nil {
		framework.Logf("Cannot create migration artifact directory: %v", err)
		return
	}
	if err := os.WriteFile(filepath.Join(dir, namespace+"-"+name+"-client.log"), []byte(output), 0644); err != nil {
		framework.Logf("Cannot save migration client state: %v", err)
	}
}

func dumpMigrationGuestState(client *kubevirt.Client, namespace, name, stage string) {
	if os.Getenv("KUBEVIRT_MIGRATION_DEBUG") != "true" {
		return
	}
	vmi := &kubevirtv1.VirtualMachineInstance{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name}}
	output, err := client.RunCommand(vmi,
		"date -u --iso-8601=ns; ip -br addr; ip -4 route show table all; ip -6 route show table all; ip neigh show; ip -6 neigh show; ss -tin; ip -s link show eth0",
		10*time.Second)
	framework.Logf("Migration guest state %s/%s %s: error=%v\n%s", namespace, name, stage, err, output)
}
