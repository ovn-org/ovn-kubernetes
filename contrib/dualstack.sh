#!/usr/bin/env bash

set -euxo pipefail

if [ ! -z "$1" ] ; then
  KIND_CONFIG_LCL=$1
else
  exit 1
fi

# Update ${KIND_CONFIG_LCL} for Dual Stack. Move to seperate script.
# Add "  ipFamily: DualStack" to the networking section of kind.yaml file,
sed -i '/apiServerPort:.*/a \ \ ipFamily: DualStack' ${KIND_CONFIG_LCL}

# Copy dual stack settings into tmp file. 'sed' can only paste
# in multiple lines from a file.
cat > dualstack.txt <<END
  apiVersion: kubeadm.k8s.io/v1beta2
  featureGates:
    IPv6DualStack: true
  kind: ClusterConfiguration
END

# Search for the line with "  kind: ClusterConfiguration" Replace
# that line of code with the contents of 'dualstack.txt'.
sed -i '/  kind: ClusterConfiguration/{
s/  kind: ClusterConfiguration//g
r dualstack.txt
}' ${KIND_CONFIG_LCL}
rm -f dualstack.txt

# Copy dual stack settings into tmp file. 'sed' can only paste
# in multiple lines from a file.
cat > dualstack.txt <<END
      "feature-gates": "SCTPSupport=true,IPv6DualStack=true"
  controllerManager:
    extraArgs:
      "feature-gates": "IPv6DualStack=true"
- |
  kind: InitConfiguration
  metadata:
    name: config
  nodeRegistration:
    kubeletExtraArgs:
      "feature-gates": "IPv6DualStack=true,CSIMigration=false"
- |
  kind: KubeletConfiguration
  featureGates:
    IPv6DualStack: true
END

# Search for the line with "feature-gates": "SCTPSupport=true" Replace
# that line of code with the contents of 'dualstack.txt'.
sed -i '/      "feature-gates": "SCTPSupport=true"/{
s/      "feature-gates": "SCTPSupport=true"//g
r dualstack.txt
}' ${KIND_CONFIG_LCL}
rm -f dualstack.txt

# Remove blank line introduced by previous two command
sed -i '/^$/d' ${KIND_CONFIG_LCL}

exit 0
