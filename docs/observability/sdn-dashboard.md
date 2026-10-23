# SDN Dashboards

This helm chart installs the dashboards for ovn-kubernetes. The dashboards are installed for the following SDN components:
  - OVN Host / Controller
  - Host / OVS
  - OVN K8S / Node Agent
  - OVN K8S / Cluster Manager

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



Before installing this Helm chart, Prometheus and Grafana must be installed.

## Installing prometheus-grafana helm chart

The [kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack) helm chart installs prometheus and grafana.

```
helm repo add prometheus-community https://prometheus-community.github.io/helm-charts
helm install kube-prometheus-stack prometheus-community/kube-prometheus-stack -n monitoring --create-namespace
```

By default this chart installs additional, dependent charts:
- [prometheus-community/kube-state-metrics](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-state-metrics)
- [prometheus-community/prometheus-node-exporter](https://github.com/prometheus-community/helm-charts/tree/main/charts/prometheus-node-exporter)
- [grafana/grafana](https://github.com/grafana/helm-charts/tree/main/charts/grafana)

From the repository root, install the dashboards in the same namespace:

```
helm install sdn-dashboard helm/sdn-dashboard -n monitoring
```

When installing OVN-Kubernetes, set `monitoring.enableServiceMonitor=true` to
create the ServiceMonitors needed for metric scraping. The chart's default
ServiceMonitor label, `release: kube-prometheus-stack`, matches the Prometheus
release name used above.

## Port Forwarding to access Grafana and Prometheus UI

```
kubectl -n monitoring port-forward deployment/kube-prometheus-stack-grafana 3000:3000

kubectl -n monitoring port-forward prometheus-kube-prometheus-stack-prometheus-0 9090:9090
```

### Credentials to access the dashboard

Username: `admin`

Password: `prom-operator`
