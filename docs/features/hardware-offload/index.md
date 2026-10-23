---
title: Hardware Acceleration
hide:
  - navigation
  - toc
---

# Hardware Acceleration

Offload OVS datapath processing to SmartNICs and DPUs.

<div class="landing-grid" markdown>

<div class="landing-card" markdown>

## OVS Acceleration with kernel datapath

Offload OVS flow processing to SmartNIC hardware using the kernel TC datapath.

[Read more](ovs-kernel.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## OVS Acceleration with DOCA datapath

Offload OVS to NVIDIA BlueField DPUs using the DOCA-based datapath.

[Read more](ovs-doca.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## DPU Support

Run OVN-Kubernetes with DPU offload for enhanced acceleration and security.

[Read more](dpu-support.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## Derive from Management Port

Auto-discover the gateway PF interface from the management port VF in DPU deployments.

[Read more](derive-from-mgmt-port.md){ .landing-btn }

</div>

<div class="landing-card" markdown>

## DPU Gateway Interface

Configure automatic gateway interface resolution from PCI address on DPU hosts.

[Read more](dpu-gateway-interface.md){ .landing-btn }

</div>

</div>
