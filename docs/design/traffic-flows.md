# Traffic Flows

In interconnect mode each node is its own OVN zone (by default).
Zones are stitched together via a **transit switch** using GENEVE tunnels
over the physical underlay.

> **Scope:** Diagrams and path narratives on this page use **IPv4** addresses
> for readability. Dual-stack and IPv6-only clusters follow the same logical
> component paths (logical switches, `ovn_cluster_router`, gateway routers,
> transit switch, management port, and host masquerade); substitute the IPv6
> defaults from the subnet table below. IPv6-specific service VIP, EgressIP,
> and host-masquerade address details are out of scope for these diagrams.

## Topology Reference

The examples below use a two-node IPv4 cluster, each node in its own zone:

| Component | ovn-worker | ovn-worker2 |
|---|---|---|
| Node / Underlay IP | 172.19.0.2 | 172.19.0.3 |
| External Switch | ext_ovn-worker | ext_ovn-worker2 |
| Gateway Router | GR_ovn-worker | GR_ovn-worker2 |
| Join Switch IP | 100.64.0.2/16 | 100.64.0.3/16 |
| Transit Switch IP | 100.88.0.3/16 | 100.88.0.4/16 |
| Pod Subnet | 10.244.0.0/24 | 10.244.2.0/24 |
| Management Port (ovn-k8s-mp0) | 10.244.0.2 | 10.244.2.2 |

### Default Internal Subnets

| Subnet | IPv4 | IPv6 | Purpose |
|---|---|---|---|
| Join | `100.64.0.0/16` | `fd98::/64` | Connects ovn_cluster_router ↔ gateway routers |
| Transit | `100.88.0.0/16` | `fd97::/64` | Interconnects zones across the cluster |
| Masquerade | `169.254.169.0/29` | `fd69::/125` | Host ↔ OVN service traffic masquerading |

### Full Topology Diagram

The host datapath is abstracted as `eth0` → `breth0` → External Switch.
OVS integration details (`br-int`, patch ports) are omitted from all diagrams
on this page.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 30, 'rankSpacing': 50, 'padding': 20}, 'themeVariables': {'fontSize': '20px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph TD
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ts fill:#26a69a,stroke:#00695C,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef host fill:#ab47bc,stroke:#6A1B9A,stroke-width:1.5px,color:#000000,font-weight:bold

    subgraph zone1["ovn-worker zone · 172.19.0.2"]
        eth1["eth0"]:::host --- br1["breth0"]:::host
        br1 --- es1["External Switch · ext_ovn-worker"]:::sw
        es1 --- gr1["GR_ovn-worker"]:::rtr
        gr1 ---|"100.64.0.2/16"| js1["Join Switch"]:::sw
        js1 --- cr1["ovn_cluster_router"]:::rtr
        cr1 ---|"10.244.0.1/24"| ls1["Logical Switch · 10.244.0.0/24"]:::sw
        ls1 --- mp1["ovn-k8s-mp0 · 10.244.0.2"]:::host
    end

    subgraph zone2["ovn-worker2 zone · 172.19.0.3"]
        eth2["eth0"]:::host --- br2["breth0"]:::host
        br2 --- es2["External Switch · ext_ovn-worker2"]:::sw
        es2 --- gr2["GR_ovn-worker2"]:::rtr
        gr2 ---|"100.64.0.3/16"| js2["Join Switch"]:::sw
        js2 --- cr2["ovn_cluster_router"]:::rtr
        cr2 ---|"10.244.2.1/24"| ls2["Logical Switch · 10.244.2.0/24"]:::sw
        ls2 --- mp2["ovn-k8s-mp0 · 10.244.2.2"]:::host
    end

    transit["Transit Switch · 100.88.0.0/16"]:::ts
    cr1 ---|"100.88.0.3/16"| transit
    cr2 ---|"100.88.0.4/16"| transit
```

---

## East-West Traffic Flows

### Pod to Pod – Same Node

Both pods are on **ovn-worker** and attach to the same node-local logical
switch. Delivery uses an **L2 lookup** at the egress stage — traffic never
reaches `ovn_cluster_router` and no tunnel is used.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 30, 'rankSpacing': 60, 'padding': 15}, 'themeVariables': {'fontSize': '30px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold

    A["Pod A · 10.244.0.5"]:::pod -->|"1 · egress"| LS["Logical Switch · L2 lookup"]:::sw
    LS -->|"2 · deliver"| B["Pod B · 10.244.0.8"]:::pod
```

---

### Pod to Pod – Different Node

Pod A is on **ovn-worker**, Pod B is on **ovn-worker2**. Traffic crosses
zones via the **transit switch** over a GENEVE tunnel on the underlay.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 60, 'rankSpacing': 50, 'padding': 35}, 'themeVariables': {'fontSize': '70px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ts fill:#26a69a,stroke:#00695C,stroke-width:1.5px,color:#000000,font-weight:bold

    A["Pod A · 10.244.0.5"]:::pod -->|"1"| LS1["Logical Switch"]:::sw
    LS1 -->|"2"| CR1["ovn_cluster_router"]:::rtr
    CR1 -->|"3 · GENEVE"| TS["Transit Switch"]:::ts
    TS -->|"4"| CR2["ovn_cluster_router"]:::rtr
    CR2 -->|"5"| LS2["Logical Switch"]:::sw
    LS2 -->|"6"| B["Pod B · 10.244.2.5"]:::pod
```

---

### Pod to ClusterIP (ovn pod backend)

The OVN load balancer on the **logical switch** DNATs the ClusterIP VIP
to a backend pod endpoint. After DNAT, normal pod-to-pod routing applies.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 30, 'rankSpacing': 60, 'padding': 15}, 'themeVariables': {'fontSize': '40px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ts fill:#26a69a,stroke:#00695C,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef decision fill:#ffca28,stroke:#F57F17,stroke-width:1.5px,color:#000000,font-weight:bold

    P["Pod · 10.244.0.5"]:::pod -->|"1 · dst: ClusterIP"| LS["Logical Switch · LB: DNAT"]:::sw
    LS -->|"2"| CR["ovn_cluster_router"]:::rtr
    CR --> DECISION{{"Endpoint local?"}}:::decision
    DECISION -->|"Same node"| LSL["Logical Switch → Local Pod"]:::pod
    DECISION -->|"Different node"| TS["Transit Switch → ovn_cluster_router → Logical Switch → Remote Pod"]:::ts
```

---

### Pod to LoadBalancerIP/ExternalIP Service (ovn pod backend)

A pod inside the cluster targets a Kubernetes **Service** of type
`LoadBalancer` (via its `status.loadBalancer.ingress` VIP) or a Service
with an `externalIPs` entry. From the pod's perspective these VIPs are
handled the same way as ClusterIP — the OVN load balancer on the logical
switch DNATs the VIP to a backend pod endpoint.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 40, 'rankSpacing': 60, 'padding': 15}, 'themeVariables': {'fontSize': '40px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ts fill:#26a69a,stroke:#00695C,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef decision fill:#ffca28,stroke:#F57F17,stroke-width:1.5px,color:#000000,font-weight:bold

    P["Pod · 10.244.0.5"]:::pod -->|"1 · dst: LB/ExternalIP"| LS["Logical Switch · LB: DNAT"]:::sw
    LS -->|"2"| CR["ovn_cluster_router"]:::rtr
    CR --> DECISION{{"Endpoint local?"}}:::decision
    DECISION -->|"Same node"| LSL["Logical Switch → Local Pod"]:::pod
    DECISION -->|"Different node"| TS["Transit Switch → ovn_cluster_router → Logical Switch → Remote Pod"]:::ts
```

---

### Pod to NodePort (ovn pod backend) – Same Node NodePort Access

Pod accesses `<nodeIP>:<nodePort>` on the **same node** it runs on.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 30, 'rankSpacing': 60, 'padding': 15}, 'themeVariables': {'fontSize': '30px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold

    P["Pod · 10.244.0.5"]:::pod -->|"1 · dst: NodePort"| LS["Logical Switch · LB: DNAT"]:::sw
    LS -->|"2"| CR["ovn_cluster_router"]:::rtr
    CR -->|"3"| LS
    LS -->|"4 · deliver"| EP["Endpoint Pod · 10.244.0.8"]:::pod
```

---

### Pod to NodePort (ovn pod backend) – Different Node NodePort Access

Pod accesses `<remoteNodeIP>:<nodePort>` on a **different node**. The
destination is the remote node's IP, so traffic egresses the local node
via the GR (SNAT to local node IP) and travels over the **physical
underlay** — not via GENEVE / transit switch. On the remote node, the
packet enters through breth0 → External Switch → GR, where the GR's
load balancer performs the DNAT to the backend pod endpoint.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 60, 'rankSpacing': 50, 'padding': 35}, 'themeVariables': {'fontSize': '70px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold

    P["Pod · 10.244.0.5"]:::pod -->|"1"| LS1["Logical Switch"]:::sw
    LS1 -->|"2"| CR1["ovn_cluster_router"]:::rtr
    CR1 -->|"3"| JS1["Join Switch"]:::sw
    JS1 -->|"4"| GR1["GR · SNAT"]:::rtr
    GR1 -->|"5"| ES1["External Switch"]:::sw
    ES1 -->|"6"| BR1["breth0 → eth0"]:::sw
    BR1 -->|"7 · underlay"| BR2["eth0 → breth0 (remote)"]:::sw
    BR2 -->|"8"| ES2["External Switch"]:::sw
    ES2 -->|"9"| GR2["GR · LB DNAT"]:::rtr
    GR2 -->|"10"| JS2["Join Switch"]:::sw
    JS2 -->|"11"| CR2["ovn_cluster_router"]:::rtr
    CR2 -->|"12"| LS2["Logical Switch"]:::sw
    LS2 -->|"13"| EP["Endpoint Pod · 10.244.2.5"]:::pod
```

---

### Pod to ClusterIP/LoadBalancerIP/ExternalIP (host networked pod backend) – Same Node

The endpoint is a **host-networked pod on the same node**. After DNAT,
`ovn_cluster_router` delivers the packet back to the logical switch and out
the **management port** (`ovn-k8s-mp0`) to the host — it does not traverse the
join switch or gateway router.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 40, 'rankSpacing': 40, 'padding': 20}, 'themeVariables': {'fontSize': '30px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef host fill:#ab47bc,stroke:#6A1B9A,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef mgmt fill:#78909c,stroke:#37474F,stroke-width:1.5px,color:#000000,font-weight:bold

    P["Pod"]:::pod -->|"1 · dst: ClusterIP"| LS["Logical Switch · LB: DNAT"]:::sw
    LS -->|"2"| CR["ovn_cluster_router"]:::rtr
    CR -->|"3"| LS
    LS -->|"4"| MP["ovn-k8s-mp0"]:::mgmt
    MP -->|"5"| HNP["Host Networked Pod"]:::host
```

---

### Pod to ClusterIP/LoadBalancerIP/ExternalIP (host networked pod backend) – Different Node

The endpoint is a **host-networked pod on a different node**. Traffic
exits through the local GR (with SNAT), crosses the underlay. On the
**remote node** the destination is a host IP, so traffic is delivered
directly to the host via the `breth0` host gateway interface — it does
**not** re-enter the OVN pipeline.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 60, 'rankSpacing': 50, 'padding': 35}, 'themeVariables': {'fontSize': '70px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef host fill:#ab47bc,stroke:#6A1B9A,stroke-width:1.5px,color:#000000,font-weight:bold

    P["Pod"]:::pod -->|"1 · dst: ClusterIP"| LS["Logical Switch · LB: DNAT"]:::sw
    LS -->|"2"| CR["ovn_cluster_router"]:::rtr
    CR -->|"3"| JS["Join Switch"]:::sw
    JS -->|"4"| GR["GR · SNAT"]:::rtr
    GR -->|"5"| ES["External Switch"]:::sw
    ES -->|"6"| BR["breth0 → eth0"]:::sw
    BR -->|"7 · underlay"| BR2["eth0 → breth0 (remote)"]:::sw
    BR2 -->|"8 · host gateway"| HNP["Host Networked Pod"]:::host
```

---

### Pod to NodePort (host networked pod backend) – Same Node NodePort Access

This scenario occurs when a Kubernetes Service with a NodePort selects
host-networked pods as backends (e.g. infrastructure DaemonSets exposed
via a Service for uniform service discovery). The datapath is the same as
the ClusterIP → host-networked case — delivery is via the management port.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 40, 'rankSpacing': 40, 'padding': 20}, 'themeVariables': {'fontSize': '30px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef host fill:#ab47bc,stroke:#6A1B9A,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef mgmt fill:#78909c,stroke:#37474F,stroke-width:1.5px,color:#000000,font-weight:bold

    P["Pod"]:::pod -->|"1 · dst: NodePort"| LS["Logical Switch · LB: DNAT"]:::sw
    LS -->|"2"| CR["ovn_cluster_router"]:::rtr
    CR -->|"3"| LS
    LS -->|"4"| MP["ovn-k8s-mp0"]:::mgmt
    MP -->|"5"| HNP["Host Networked Pod"]:::host
```

---

### Pod to NodePort (host networked pod backend) – Different Node NodePort Access

Same scenario (Service with NodePort selecting host-networked pods),
but the backend is on a **remote node**. The OVN load balancer DNATs
locally to the host-networked pod IP. Since this is a host IP, traffic
exits through the GR (with SNAT), crosses the **physical underlay** —
not GENEVE. On the remote node, traffic is delivered directly to the
host via the `breth0` host gateway interface — it does **not** re-enter
the OVN pipeline.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 60, 'rankSpacing': 50, 'padding': 35}, 'themeVariables': {'fontSize': '70px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef host fill:#ab47bc,stroke:#6A1B9A,stroke-width:1.5px,color:#000000,font-weight:bold

    P["Pod"]:::pod -->|"1 · dst: NodePort"| LS["Logical Switch · LB: DNAT"]:::sw
    LS -->|"2"| CR["ovn_cluster_router"]:::rtr
    CR -->|"3"| JS["Join Switch"]:::sw
    JS -->|"4"| GR["GR · SNAT"]:::rtr
    GR -->|"5"| ES["External Switch"]:::sw
    ES -->|"6"| BR["breth0 → eth0"]:::sw
    BR -->|"7 · underlay"| BR2["eth0 → breth0 (remote)"]:::sw
    BR2 -->|"8 · host gateway"| HNP["Host Networked Pod"]:::host
```

---

## Egress Traffic Flows

### Pod to External: routingViaHost=false

Default shared gateway mode. Egress goes entirely through the OVN
pipeline; the GR SNATs the pod IP to the node IP.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 60, 'rankSpacing': 50, 'padding': 35}, 'themeVariables': {'fontSize': '70px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ext fill:#ef5350,stroke:#B71C1C,stroke-width:1.5px,color:#000000,font-weight:bold

    P["Pod"]:::pod -->|"1"| LS["Logical Switch"]:::sw
    LS -->|"2"| CR["ovn_cluster_router"]:::rtr
    CR -->|"3"| JS["Join Switch"]:::sw
    JS -->|"4"| GR["GR · SNAT"]:::rtr
    GR -->|"5"| ES["External Switch"]:::sw
    ES -->|"6"| BR["breth0"]:::sw
    BR -->|"7"| ETH["eth0"]:::sw
    ETH -->|"8"| EXT["External"]:::ext
```

---

### Pod to External: routingViaHost=true

Local gateway mode. Egress leaves OVN via the **management port**
(`ovn-k8s-mp0`) into the host networking stack. The host routing table
selects the egress interface and iptables/nftables masquerade SNATs to the
node IP. Traffic does **not** traverse the gateway router for SNAT.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 40, 'rankSpacing': 40, 'padding': 20}, 'themeVariables': {'fontSize': '30px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef host fill:#ab47bc,stroke:#6A1B9A,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef mgmt fill:#78909c,stroke:#37474F,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ext fill:#ef5350,stroke:#B71C1C,stroke-width:1.5px,color:#000000,font-weight:bold

    P["Pod"]:::pod -->|"1"| LS["Logical Switch"]:::sw
    LS -->|"2"| CR["ovn_cluster_router"]:::rtr
    CR -->|"3"| LS
    LS -->|"4"| MP["ovn-k8s-mp0"]:::mgmt
    MP -->|"5"| HOST["Host · masquerade"]:::host
    HOST -->|"6"| ETH["eth0"]:::sw
    ETH -->|"7"| EXT["External"]:::ext
```

---

### EgressIP Pod to External – Same Node; EIP on primary br-ex shared network

The GR SNATs to the EgressIP instead of the node IP.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 60, 'rankSpacing': 50, 'padding': 35}, 'themeVariables': {'fontSize': '70px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ext fill:#ef5350,stroke:#B71C1C,stroke-width:1.5px,color:#000000,font-weight:bold

    P["Pod"]:::pod -->|"1"| LS["Logical Switch"]:::sw
    LS -->|"2"| CR["ovn_cluster_router"]:::rtr
    CR -->|"3"| JS["Join Switch"]:::sw
    JS -->|"4"| GR["GR · SNAT → EgressIP"]:::rtr
    GR -->|"5"| ES["External Switch · breth0"]:::sw
    ES -->|"6"| ETH["eth0"]:::sw
    ETH -->|"7 · SNAT: EgressIP"| EXT["External"]:::ext
```

---

### EgressIP Pod to External – Same Node; EIP on secondary network

When the EgressIP is on a **secondary host interface**, `ovn_cluster_router`
reroutes the packet to the local management port. The host then applies an
iptables SNAT (`OVN-KUBE-EGRESS-IP-MULTI-NIC`) to the EgressIP and an ip-rule
steers the packet out the secondary NIC. No NAT is performed on the gateway
router.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 80, 'rankSpacing': 50, 'padding': 65}, 'themeVariables': {'fontSize': '70px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef host fill:#ab47bc,stroke:#6A1B9A,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef mgmt fill:#78909c,stroke:#37474F,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ext fill:#ef5350,stroke:#B71C1C,stroke-width:1.5px,color:#000000,font-weight:bold

    P["Pod"]:::pod -->|"1"| LS["Logical Switch"]:::sw
    LS -->|"2"| CR["ovn_cluster_router · reroute"]:::rtr
    CR -->|"3"| LS
    LS -->|"4"| MP["ovn-k8s-mp0"]:::mgmt
    MP -->|"5"| HOST["Host · iptables SNAT → EgressIP"]:::host
    HOST -->|"6"| ETH2["Secondary NIC"]:::sw
    ETH2 -->|"7 · SNAT: EgressIP"| EXT["External"]:::ext
```

---

### EgressIP Pod to Node IP within the cluster

When an EgressIP pod sends traffic to another **node IP** within the
cluster, the EgressIP is **not** used as the source IP. EgressIP only
applies to destinations outside the cluster's node network.

#### Shared gateway mode (`routingViaHost=false`)

The GR performs a regular SNAT to the node IP (same as non-EgressIP
egress).

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 60, 'rankSpacing': 50, 'padding': 35}, 'themeVariables': {'fontSize': '70px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef mgmt fill:#78909c,stroke:#37474F,stroke-width:1.5px,color:#000000,font-weight:bold

    P["Pod"]:::pod -->|"1"| LS["Logical Switch"]:::sw
    LS -->|"2"| CR["ovn_cluster_router"]:::rtr
    CR -->|"3"| JS["Join Switch"]:::sw
    JS -->|"4"| GR["GR · SNAT → Node IP"]:::rtr
    GR -->|"5"| ES["External Switch"]:::sw
    ES -->|"6"| BR["breth0 → eth0"]:::sw
    BR -->|"7 · SNAT: Node IP"| DST["ovn-worker2"]:::mgmt
```

#### Local gateway mode (`routingViaHost=true`)

Egress leaves OVN via the management port. The host networking stack
performs the SNAT to the node IP via iptables/nftables masquerade (same
as non-EgressIP local gateway egress).

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 60, 'rankSpacing': 50, 'padding': 35}, 'themeVariables': {'fontSize': '50px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef host fill:#ab47bc,stroke:#6A1B9A,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef mgmt fill:#78909c,stroke:#37474F,stroke-width:1.5px,color:#000000,font-weight:bold

    P["Pod"]:::pod -->|"1"| LS["Logical Switch"]:::sw
    LS -->|"2"| CR["ovn_cluster_router"]:::rtr
    CR -->|"3"| LS
    LS -->|"4"| MP["ovn-k8s-mp0"]:::mgmt
    MP -->|"5"| HOST["Host · masquerade SNAT → Node IP"]:::host
    HOST -->|"6"| ETH["eth0"]:::sw
    ETH -->|"7 · SNAT: Node IP"| DST["ovn-worker2"]:::mgmt
```

---

### EgressIP Pod to External – Different Node; EIP on primary br-ex shared network

Traffic is **rerouted** to the EgressIP node via the transit switch,
then exits from that node's GR.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 60, 'rankSpacing': 50, 'padding': 35}, 'themeVariables': {'fontSize': '70px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ts fill:#26a69a,stroke:#00695C,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ext fill:#ef5350,stroke:#B71C1C,stroke-width:1.5px,color:#000000,font-weight:bold

    P["Pod"]:::pod -->|"1"| LS1["Logical Switch"]:::sw
    LS1 -->|"2"| CR1["ovn_cluster_router"]:::rtr
    CR1 -->|"3 · reroute"| TS["Transit Switch"]:::ts
    TS -->|"4"| CR2["ovn_cluster_router"]:::rtr
    CR2 -->|"5"| JS2["Join Switch"]:::sw
    JS2 -->|"6"| GR2["GR · SNAT → EgressIP"]:::rtr
    GR2 -->|"7"| ES2["External Switch"]:::sw
    ES2 -->|"8 · SNAT: EgressIP"| EXT["External"]:::ext
```

---

### EgressIP Pod to External – Different Node; EIP on secondary network

Traffic is **rerouted** to the EgressIP node via the transit switch. On that
node, `ovn_cluster_router` sends the packet out the management port; the host
performs iptables SNAT to the EgressIP and steers it onto the secondary NIC.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 60, 'rankSpacing': 50, 'padding': 35}, 'themeVariables': {'fontSize': '70px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ts fill:#26a69a,stroke:#00695C,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef host fill:#ab47bc,stroke:#6A1B9A,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef mgmt fill:#78909c,stroke:#37474F,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ext fill:#ef5350,stroke:#B71C1C,stroke-width:1.5px,color:#000000,font-weight:bold

    P["Pod"]:::pod -->|"1"| LS1["Logical Switch"]:::sw
    LS1 -->|"2"| CR1["ovn_cluster_router"]:::rtr
    CR1 -->|"3 · reroute"| TS["Transit Switch"]:::ts
    TS -->|"4"| CR2["ovn_cluster_router · reroute"]:::rtr
    CR2 -->|"5"| LS2["Logical Switch"]:::sw
    LS2 -->|"6"| MP["ovn-k8s-mp0"]:::mgmt
    MP -->|"7"| HOST["Host · iptables SNAT → EgressIP"]:::host
    HOST -->|"8"| ETH2["Secondary NIC"]:::sw
    ETH2 -->|"9 · SNAT: EgressIP"| EXT["External"]:::ext
```

---

## Ingress Traffic Flows

### External to NodePort/ExternalIP/LoadBalancerIP (ovn pod backend) – Same Node: routingViaHost=false

External traffic enters the node where the **backend pod is local**.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 60, 'rankSpacing': 50, 'padding': 35}, 'themeVariables': {'fontSize': '70px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ext fill:#ef5350,stroke:#B71C1C,stroke-width:1.5px,color:#000000,font-weight:bold

    EXT["External Client"]:::ext -->|"1"| ETH["eth0"]:::sw
    ETH -->|"2"| BR["breth0"]:::sw
    BR -->|"3"| ES["External Switch"]:::sw
    ES -->|"4"| GR["GR · LB DNAT"]:::rtr
    GR -->|"5"| JS["Join Switch"]:::sw
    JS -->|"6"| CR["ovn_cluster_router"]:::rtr
    CR -->|"7"| LS["Logical Switch"]:::sw
    LS -->|"8"| P["Pod · 10.244.0.5"]:::pod
```

---

### External to NodePort/ExternalIP/LoadBalancerIP (ovn pod backend) – Different Node: routingViaHost=false

External traffic enters one node but the backend pod is on a
**different node**. After DNAT, traffic crosses zones via the transit switch.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 60, 'rankSpacing': 50, 'padding': 35}, 'themeVariables': {'fontSize': '70px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ts fill:#26a69a,stroke:#00695C,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ext fill:#ef5350,stroke:#B71C1C,stroke-width:1.5px,color:#000000,font-weight:bold

    EXT["External"]:::ext -->|"1"| ETH["eth0"]:::sw
    ETH -->|"2"| BR["breth0"]:::sw
    BR -->|"3"| ES["Ext Switch"]:::sw
    ES -->|"4"| GR["GR · DNAT"]:::rtr
    GR -->|"5"| JS["Join Switch"]:::sw
    JS -->|"6"| CR1["ovn_cluster_router"]:::rtr
    CR1 -->|"7 · GENEVE"| TS["Transit Switch"]:::ts
    TS -->|"8"| CR2["ovn_cluster_router"]:::rtr
    CR2 -->|"9"| LS2["Logical Switch"]:::sw
    LS2 -->|"10"| P["Pod"]:::pod
```

---

### External to NodePort/ExternalIP/LoadBalancerIP (ovn pod backend) – Same Node: routingViaHost=true

Local gateway mode with default `externalTrafficPolicy: Cluster`. External
traffic enters the host via breth0, where nftables DNATs the NodePort /
ExternalIP / LoadBalancerIP to its **ClusterIP**. The packet is then
routed back into `breth0` and enters OVN via the GR, where the GR load
balancer performs the final DNAT to a backend pod endpoint.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 60, 'rankSpacing': 50, 'padding': 35}, 'themeVariables': {'fontSize': '50px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef host fill:#ab47bc,stroke:#6A1B9A,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ext fill:#ef5350,stroke:#B71C1C,stroke-width:1.5px,color:#000000,font-weight:bold

    EXT["External"]:::ext -->|"1"| ETH["eth0"]:::sw
    ETH -->|"2"| BR["breth0"]:::sw
    BR -->|"3"| HOST["Host · nftables DNAT → ClusterIP"]:::host
    HOST -->|"4"| BR2["breth0"]:::sw
    BR2 -->|"5"| GR["GR · LB DNAT"]:::rtr
    GR -->|"6"| JS["Join Switch"]:::sw
    JS -->|"7"| CR["ovn_cluster_router"]:::rtr
    CR -->|"8"| LS["Logical Switch"]:::sw
    LS -->|"9"| P["Pod · 10.244.0.5"]:::pod
```

---

### External to NodePort/ExternalIP/LoadBalancerIP (ovn pod backend) – Different Node: routingViaHost=true

Local gateway mode with default `externalTrafficPolicy: Cluster`, backend
on a remote node. The host nftables DNAT translates to ClusterIP, the
packet is routed back into breth0, and the GR load balancer performs the
final DNAT. Since the backend is remote, traffic crosses zones via the
transit switch.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 60, 'rankSpacing': 50, 'padding': 35}, 'themeVariables': {'fontSize': '70px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ts fill:#26a69a,stroke:#00695C,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef host fill:#ab47bc,stroke:#6A1B9A,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ext fill:#ef5350,stroke:#B71C1C,stroke-width:1.5px,color:#000000,font-weight:bold

    EXT["External"]:::ext -->|"1"| ETH["eth0"]:::sw
    ETH -->|"2"| BR["breth0"]:::sw
    BR -->|"3"| HOST["Host · nftables DNAT → ClusterIP"]:::host
    HOST -->|"4"| BR2["breth0"]:::sw
    BR2 -->|"5"| GR["GR · LB DNAT"]:::rtr
    GR -->|"6"| JS["Join Switch"]:::sw
    JS -->|"7"| CR1["ovn_cluster_router"]:::rtr
    CR1 -->|"8 · GENEVE"| TS["Transit Switch"]:::ts
    TS -->|"9"| CR2["ovn_cluster_router"]:::rtr
    CR2 -->|"10"| LS2["Logical Switch"]:::sw
    LS2 -->|"11"| P["Pod"]:::pod
```

---

### External to NodePort/ExternalIP/LoadBalancer (ovn pod backend) ETP=local

#### Shared gateway mode (`routingViaHost=false`)

With `externalTrafficPolicy: Local`, only **local endpoints** receive
ingress traffic (preserving client source IP). A special `skip_snat`
load balancer is used on the GR to preserve the client IP.

**Ingress path:**

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 60, 'rankSpacing': 50, 'padding': 35}, 'themeVariables': {'fontSize': '60px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ext fill:#ef5350,stroke:#B71C1C,stroke-width:1.5px,color:#000000,font-weight:bold

    EXT["External"]:::ext -->|"1"| ETH["eth0"]:::sw
    ETH -->|"2"| BR["breth0"]:::sw
    BR -->|"3"| ES["Ext Switch"]:::sw
    ES -->|"4"| GR["GR · DNAT · skip_snat · ETP=local"]:::rtr
    GR -->|"5 · client IP preserved"| JS["Join Switch"]:::sw
    JS -->|"6"| CR["ovn_cluster_router"]:::rtr
    CR -->|"7"| LS["Logical Switch"]:::sw
    LS -->|"8"| P["Local Pod"]:::pod
```

**Reply path:** The reply is **not** SNAT'd. Because `skip_snat` was
used on ingress, the GR performs only a conntrack **reverse DNAT**
(unDNAT): the pod's source address is translated back to the original
service IP + service port. The client IP in the destination is unchanged
throughout. EgressIP does not apply to service reply traffic — it only
affects pod-initiated outbound connections.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 60, 'rankSpacing': 50, 'padding': 35}, 'themeVariables': {'fontSize': '70px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ext fill:#ef5350,stroke:#B71C1C,stroke-width:1.5px,color:#000000,font-weight:bold

    P["Pod reply"]:::pod -->|"1"| LS["Logical Switch"]:::sw
    LS -->|"2"| CR["ovn_cluster_router"]:::rtr
    CR -->|"3"| JS["Join Switch"]:::sw
    JS -->|"4"| GR["GR · unDNAT → svc IP:port"]:::rtr
    GR -->|"5"| ES["External Switch"]:::sw
    ES -->|"6"| BR["breth0 → eth0"]:::sw
    BR -->|"7"| EXT["External Client"]:::ext
```

#### Local gateway mode (`routingViaHost=true`)

In local gateway mode with ETP=Local, host nftables DNATs the NodePort to
a special masquerade IP (`169.254.169.3`) and forwards into `ovn-k8s-mp0`.
A dedicated load balancer on the logical switch translates the masquerade
VIP to local-only backends. Client IP is preserved because iptables skips
SNAT on `ovn-k8s-mp0` for this flow.

> **Note:** This path applies to services that allocate NodePorts (the
> default). A `LoadBalancer` Service with
> `allocateLoadBalancerNodePorts: false` bypasses the NodePort-based
> logical-switch load balancer; in that case host nftables DNATs directly
> to the local endpoint without the masquerade VIP detour.

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 60, 'rankSpacing': 50, 'padding': 35}, 'themeVariables': {'fontSize': '50px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph LR
    classDef pod fill:#66bb6a,stroke:#2E7D32,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef host fill:#ab47bc,stroke:#6A1B9A,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef mgmt fill:#78909c,stroke:#37474F,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ext fill:#ef5350,stroke:#B71C1C,stroke-width:1.5px,color:#000000,font-weight:bold

    EXT["External"]:::ext -->|"1"| ETH["eth0"]:::sw
    ETH -->|"2"| BR["breth0"]:::sw
    BR -->|"3"| HOST["Host · nftables DNAT → 169.254.169.3"]:::host
    HOST -->|"4"| MP["ovn-k8s-mp0"]:::mgmt
    MP -->|"5"| LS["Logical Switch · LB: masq VIP DNAT"]:::sw
    LS -->|"6"| P["Local Pod"]:::pod
```

---

## Summary — Component Roles

<div style="text-align: center;">

```mermaid
%%{init: {'theme': 'base', 'flowchart': {'nodeSpacing': 30, 'rankSpacing': 50, 'padding': 15}, 'themeVariables': {'fontSize': '16px', 'primaryTextColor': '#000000', 'secondaryTextColor': '#000000', 'tertiaryTextColor': '#000000', 'textColor': '#000000', 'nodeTextColor': '#000000'}}}%%
graph TD
    classDef sw fill:#42a5f5,stroke:#1565C0,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef rtr fill:#ffa726,stroke:#E65100,stroke-width:1.5px,color:#000000,font-weight:bold
    classDef ts fill:#26a69a,stroke:#00695C,stroke-width:1.5px,color:#000000,font-weight:bold

    LS["Logical Switch · per node"]:::sw -->|"routed pod traffic + LB DNAT"| CR["ovn_cluster_router · per zone"]:::rtr
    CR -->|"intra-zone routing"| LS
    CR -->|"cross-zone routing"| TS["Transit Switch · cluster-wide"]:::ts
    CR -->|"north-south routing"| JS["Join Switch"]:::sw
    JS --> GR["Gateway Router · per node · SNAT / DNAT (shared gw)"]:::rtr
    GR --> ES["External Switch · per node"]:::sw
    ES --> BRINT["breth0 ↔ eth0"]:::sw
```

</div>

| Component | Scope | Role |
|---|---|---|
| **Logical Switch** | Node-local | An OVN logical switch emulates an L2 Ethernet switch. Each node has one logical switch that hosts all pod logical ports on that node. Same-node pod-to-pod traffic is delivered entirely within the logical switch via L2 lookup without reaching `ovn_cluster_router`. OVN load balancers are attached here to perform service DNAT (ClusterIP, NodePort, LoadBalancerIP, ExternalIP); after DNAT, routed traffic enters the routing pipeline |
| **ovn_cluster_router** | Per zone | An OVN logical router that performs L3 routing for east-west pod traffic within the zone. It connects to the node-local logical switch (pod subnet), the join switch (toward the gateway router), and the transit switch (for cross-zone traffic). Routing policies here implement EgressIP reroutes and management port delivery |
| **Join Switch** | Per zone | An internal logical switch that connects `ovn_cluster_router` to the per-node gateway routers within the same zone. Traffic traversing this switch is transitioning between east-west and north-south domains |
| **Gateway Router** | Per node | An OVN logical router (one per node) that handles north-south traffic — packets entering or leaving the cluster. In shared gateway mode it performs SNAT on egress (pod IP → node IP, or pod IP → EgressIP when the EgressIP is on the primary `breth0` network) and DNAT on ingress (VIP → pod IP). For EgressIP on a secondary network, SNAT happens in the host iptables stack instead (traffic exits via `ovn-k8s-mp0`, bypassing the GR). In local gateway mode (`routingViaHost=true`) ordinary pod egress SNAT happens in the host network stack (iptables/nftables), but service traffic can still undergo load-balancer DNAT in OVN on the gateway router or logical switch depending on the path, and EgressIP SNAT also remains on the gateway router when the EgressIP is assigned to the primary network attached to the OVS gateway bridge |
| **External Switch** | Per node | An OVN logical switch that bridges the gateway router's external-facing port to the physical network via the OVS bridge `breth0`. It carries traffic between the gateway router and the node's physical NIC |
| **Transit Switch** | Cluster-wide | An OVN logical switch that interconnects zones (one per cluster in interconnect mode). Each zone's `ovn_cluster_router` has a port on this switch. Cross-zone traffic is encapsulated in GENEVE tunnels over the physical underlay between nodes |
| **ovn-k8s-mp0** | Per node | The management port — a logical switch port whose peer is a Linux interface (`ovn-k8s-mp0`) in the host network namespace. It enables host ↔ OVN connectivity: host-networked pod delivery, local gateway egress, and health-check traffic between the host and OVN |
