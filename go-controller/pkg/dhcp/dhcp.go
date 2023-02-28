package dhcp

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/ovn-org/ovn-kubernetes/go-controller/pkg/nbdb"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clientset "k8s.io/client-go/kubernetes"
)

func ComposeOptionsWithKubeDNS(k8scli clientset.Interface, hostname string, cidr string, router string) (*nbdb.DHCPOptions, error) {
	dnsServer, err := kubeDNSNameServer(k8scli)
	if err != nil {
		return nil, err
	}

	dhcpOptions := nbdb.DHCPOptions{
		Cidr: cidr,
		Options: map[string]string{
			"lease_time": "3500", /*TODO: Configure it*/
			"router":     router,
			"dns_server": dnsServer,
			"server_id":  router,
			"server_mac": "c0:ff:ee:00:00:01", /*TODO: Generate it*/
			"hostname":   fmt.Sprintf("%q", hostname),
		},
	}

	return &dhcpOptions, nil
}

func kubeDNSNameServer(cli clientset.Interface) (string, error) {
	svc, err := cli.CoreV1().Services("kube-system").Get(context.Background(), "kube-dns", metav1.GetOptions{})
	if err != nil {
		if !apierrors.IsNotFound(err) {
			return "", err
		}
		svc, err = cli.CoreV1().Services("openshift-dns").Get(context.Background(), "dns-default", metav1.GetOptions{})
		if err != nil {
			return "", err
		}
	}
	return svc.Spec.ClusterIP, nil
}
