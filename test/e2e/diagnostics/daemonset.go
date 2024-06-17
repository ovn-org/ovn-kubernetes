package diagnostics

import (
	"context"
	"fmt"
	"os"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/pointer"
)

func composePeriodicCmd(cmd string, interval uint32) string {
	return fmt.Sprintf("while true; do echo \\\"=== $(date) ===\\\" && %s && sleep %d; done", cmd, interval)
}

func (d *Diagnostics) composeDiagnosticsDaemonSet(name, cmd, tool string) appsv1.DaemonSet {
	ovnImage := os.Getenv("OVN_IMAGE")
	if ovnImage == "" {
		ovnImage = "localhost/ovn-daemonset-fedora:dev"
	}
	return appsv1.DaemonSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: d.fr.Namespace.Name,
		},
		Spec: appsv1.DaemonSetSpec{
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"app": name,
				},
			},
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Name:      name,
					Namespace: d.fr.Namespace.Name,
					Labels: map[string]string{
						"app":  name,
						"tool": tool,
					},
				},
				Spec: v1.PodSpec{
					Containers: []v1.Container{{
						Name:            "ovn-kube",
						Image:           ovnImage,
						ImagePullPolicy: v1.PullIfNotPresent,
						Command:         []string{"bash", "-c"},
						Args:            []string{cmd},
						SecurityContext: &v1.SecurityContext{
							Privileged: pointer.Bool(true),
						},
						VolumeMounts: []v1.VolumeMount{
							{
								Name:      "host-run-ovs",
								MountPath: "/run/openvswitch",
							},
							{
								Name:      "host-var-run-ovs",
								MountPath: "/var/run/openvswitch",
							},
							{
								Name:      "host",
								MountPath: "/host",
							},
						},
					}},
					HostNetwork: true,
					Volumes: []v1.Volume{
						{
							Name: "host-run-ovs",
							VolumeSource: v1.VolumeSource{
								HostPath: &v1.HostPathVolumeSource{
									Path: "/run/openvswitch",
								},
							},
						},
						{
							Name: "host-var-run-ovs",
							VolumeSource: v1.VolumeSource{
								HostPath: &v1.HostPathVolumeSource{
									Path: "/var/run/openvswitch",
								},
							},
						},
						{
							Name: "host",
							VolumeSource: v1.VolumeSource{
								HostPath: &v1.HostPathVolumeSource{
									Path: "/",
								},
							},
						},
					},
				},
			},
		},
	}
}

func (d *Diagnostics) runDaemonSets(daemonSets []appsv1.DaemonSet) error {
	for _, daemonSet := range daemonSets {
		_, err := d.fr.ClientSet.AppsV1().DaemonSets(daemonSet.Namespace).Create(context.Background(), &daemonSet, metav1.CreateOptions{})
		if err != nil {
			return err
		}
	}
	for _, daemonSet := range daemonSets {
		err := wait.PollUntilContextTimeout(context.Background(), time.Second, 15*time.Second, true /*immediate*/, func(ctx context.Context) (bool, error) {
			daemonSet, err := d.fr.ClientSet.AppsV1().DaemonSets(daemonSet.Namespace).Get(ctx, daemonSet.Name, metav1.GetOptions{})
			if err != nil {
				return false, err
			}
			return daemonSet.Status.NumberAvailable == daemonSet.Status.NumberReady, nil
		})
		if err != nil {
			return err
		}
	}
	return nil
}
