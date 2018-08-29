# Community Container Image

From time to time the maintainers build and push an image to
```
docker.io/ovnkube/ovn-daemonset
```
The image may be a release version, or latest which is a stable
usable image reflecting work in progress. The daemonsets in dist/yaml
default to this image:
```
docker.io/ovnkube/ovn-daemonset:latest
```

When the imgage is started in the daemonset the log displays the
git commit number of the build.

```
# kubectl logs ovnkube-master-klpnm -c ovn-northd
==================== compatible with version 1  and version 2 daemonsets
Image built from ovn-kubernetes ref: refs/heads/ovn-containers  commit: 8468bacb56f2ca213d044dcb2b27ddee4851cdaa
```

## Building the Container Image

The community ovn container image is built as follows

```
# cd ovn-kubernetes

# docker login -u ovnkube docker.io
Password:
Login Succeeded

# docker build -f Dockerfile.centos -t ovn-daemonset .
# docker tag  ovn-daemonset docker.io/ovnkube/ovn-daemonset:latest
# docker push docker.io/ovnkube/ovn-daemonset:latest
```

