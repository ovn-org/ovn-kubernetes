# Integration Tests

This directory contains playbooks to set up for and run set of integration
tests for ovn-kubernetes using openshift-dind cluster on RHEL and Fedora hosts. One entrypoint exists:

 - `main.yml`: sets up the machine and runs tests

When running `main.yml`, two tags are present:

 - `setup`: run all tasks to set up the system for testing
 - `integration`: build ovn-kubernetes from source and run the dind based cni_vendor_tests

The playbooks assume the following things about your system:

 - on RHEL, the server and extras repos are configured and certs are present
 - `ansible` is installed and the host is boot-strapped to allow `ansible` to run against it
 - the `$GOPATH` is set and present for all shells (*e.g.* written in `/etc/environment`)
 - ovn-kubernetes repository is checked out to the correct state at `${GOPATH}/src/github.com/openvswitch/ovn-kubernetes`
 - the user running the playbook has access to passwordless `sudo`
