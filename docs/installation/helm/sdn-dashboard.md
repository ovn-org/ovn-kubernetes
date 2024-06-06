# sdn-dashboard

-----------------------

![Version: 1.0.0](https://img.shields.io/badge/Version-1.0.0-informational?style=flat-square) ![AppVersion: 1.0.0](https://img.shields.io/badge/AppVersion-1.0.0-informational?style=flat-square) 

**Homepage:** <https://www.ovn.org/>

## Source Code

* <https://github.com/ovn-org/ovn-kubernetes>

# sdn-dashboard

This helm chart installs the dashboards for ovn-kubernetes. The dashboards are installed for the following SDN components:
  - OVN Central / North Daemon
  - OVN Central / Northbound DB
  - OVN Central / Southbound DB
  - OVN Host / Controller
  - Host / OVS
  - OVN K8S / Node Agent
  - OVN K8S / Cluster Manager

```
helm install sdn-dashboard sdn-dashboard/
```


## Values

<table>
	<thead>
		<th>Key</th>
		<th>Type</th>
		<th>Default</th>
		<th>Description</th>
	</thead>
	<tbody>
		<tr>
			<td>global.enableDPUDashboards</td>
			<td>bool</td>
			<td><pre lang="json">
false
</pre>
</td>
			<td>Displays DPU panels in the dashboards if set to true</td>
		</tr>
		<tr>
			<td>global.namespace</td>
			<td>string</td>
			<td><pre lang="json">
"monitoring"
</pre>
</td>
			<td>Namespace where the dashboards are installed. Same as the namespace where prometheus and grafana are installed.</td>
		</tr>
	</tbody>
</table>



Before, installing this helm chart, prometheus and grafana must be pre-installed.

## Installing prometheus-grafana helm chart

The [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack) helm chart installs prometheus and grafana.

```
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm install kube-prometheus-stack prometheus-community/kube-prometheus-stack -n <desired_namespace> --set prometheusOperator.tls.enabled=false --set prometheusOperator.admissionWebhooks.enabled=false --set prometheusOperator.admissionWebhooks.patch.enabled=false
```

By default this chart installs additional, dependent charts:
- [prometheus-community/kube-state-metrics](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-state-metrics)
- [prometheus-community/prometheus-node-exporter](https://github.com/prometheus-community/helm-charts/tree/main/charts/prometheus-node-exporter)
- [grafana/grafana](https://github.com/grafana/helm-charts/tree/main/charts/grafana)

After configuring Prometheus and Grafana and installing sdn-dashboard helm chart, ovn-kubernetes can be installed and the metrics will be scraped.

## Port Forwarding to access Grafana and Prometheus UI

```
kubectl -n <desired_namespace> port-forward deployment/kube-prometheus-stack-grafana 3000:3000 --address 0.0.0.0

kubectl -n <desired_namespace> port-forward prometheus-kube-prometheus-stack-prometheus-0 9090:9090 --address 0.0.0.0
```

### Credentials to access the dashboard

Username: `admin`

Password: `prom-operator`
