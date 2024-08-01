#/bin/bash -xe
node=$1
shift
name=$1
shift
container=$1
shift
pod=$(kubectl get pods -n ovn-kubernetes --field-selector spec.nodeName=$node -l name=$name -o name)
kubectl exec -it -n ovn-kubernetes $pod -c $container -- $@
