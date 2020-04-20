#!/bin/bash
# utility script to generate yaml config file with dynamic number of master 
# and worker nodes to be used when creating a cluster. Rename the generated file to
# kind.yaml or kind-ha.yaml as appropriate for the use case

#PRE-REQUISITE: docker needs to be installed to run the docker image --> mikefarah/yq

# Configurable definitons and defaults. The below can be set as environment variables as well
MASTER_COUNT=${MASTER_COUNT:-1}
NODE_COUNT=${NODE_COUNT:-2}
MASTER_COUNT_MAX=${MASTER_COUNT_MAX:-3}
NODE_COUNT_MAX=${NODE_COUNT_MAX:-15}
OUTPUT_FILE=${OUTPUT_FILE:-gen_kind.yaml.GEN}

echo INPUT::master_node_count=$MASTER_COUNT
echo INPUT::worker_node_count=$NODE_COUNT
echo MODULUS::mastercount%2 = `expr $MASTER_COUNT % 2`
echo DEFAULTS::MASTER_COUNT_MAX=$MASTER_COUNT_MAX
echo DEFAULTS::NODE_COUNT_MAX=$NODE_COUNT_MAX
echo OUTPUT_FILE::Generated output filename=$OUTPUT_FILE

# validate master node count
if [[ $MASTER_COUNT -le 0 || $MASTER_COUNT -gt $MASTER_COUNT_MAX || `expr $MASTER_COUNT % 2` != 1 ]]
then
   echo "ERROR: master count valid value to be equal to between 1 and $MASTER_COUNT_MAX and should be ODD"
   exit 1
fi

# validate worker node count
if [[ $NODE_COUNT -lt 0 || $NODE_COUNT -gt $NODE_COUNT_MAX ]]
then
   echo "ERROR: worker count valid value to be between 0 and $NODE_COUNT_MAX"
   exit 1
fi

TMPDIR=$(mktemp -d)
COREFILE=kindGen.yaml
MNODEFILE=masterGen.yaml
WNODEFILE=nodeGen.yaml
echo $TMPDIR $COREFILE $MNODEFILE $WNODEFILE

cat <<EOF > $TMPDIR/$COREFILE
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  # the default CNI will not be installed
  disableDefaultCNI: true
  apiServerAddress: 11.12.13.1
  apiServerPort: 11337
kubeadmConfigPatches:
- |
  kind: ClusterConfiguration
  metadata:
    name: config
  apiServer:
    extraArgs:
      "feature-gates": "SCTPSupport=true"
nodes:
EOF

cat <<EOF > $TMPDIR/$MNODEFILE
nodes:
 - role: control-plane
   extraMounts:
     - hostPath: /tmp/kind
       containerPath: /var/run/secrets/kubernetes.io/serviceaccount/
   kubeadmConfigPatches:
   - |
     kind: InitConfiguration
     nodeRegistration:
       kubeletExtraArgs:
         node-labels: "ingress-ready=true"
         authorization-mode: "AlwaysAllow"
EOF

cat <<EOF > $TMPDIR/$WNODEFILE
nodes:
 - role: worker
   extraMounts:
     - hostPath: /tmp/kind
       containerPath: /var/run/secrets/kubernetes.io/serviceaccount/
EOF

for ((i=1;i<=$MASTER_COUNT;++i)); do docker run --rm -it -v $TMPDIR:/workdir mikefarah/yq yq m -i -a $COREFILE $MNODEFILE; done
for ((i=1;i<=$NODE_COUNT;++i)); do docker run --rm -it -v $TMPDIR:/workdir mikefarah/yq yq m -i -a $COREFILE $WNODEFILE; done
cat $TMPDIR/$COREFILE
mv $TMPDIR/$COREFILE ./$OUTPUT_FILE
rm -rf $TMPDIR

