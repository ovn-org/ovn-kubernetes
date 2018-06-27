# Copyright (C) 2018 Red Hat Inc.
#
# Copying and distribution of this file, with or without modification,
# are permitted in any medium without royalty provided the copyright
# notice and this notice are preserved.  This file is offered as-is,
# without warranty of any kind.

#
# The rpms can be made from a release version, make rpm, or from a
# commit, make snapshot. In either case the spec file needs to be
# edited for the version or commit number.
#

# Build distribution SRPM and RPMs based on version number specified
# in the openvswitch-ovn-kubernetes.spec file
rpms: srpm rpm

# Build snapshot RPMs based on the commit number in the
# the openvswitch-ovn-kubernetes.spec.snapshot file
# The commit must be in https://github.com/openvswitch/ovn-kubernetes
snapshot: rpmsnap

dist: openvswitch-ovn-kubernetes.spec
	spectool -g openvswitch-ovn-kubernetes.spec

srpm: dist
	rpmbuild --define "_sourcedir `pwd`" --define "_specdir `pwd`" \
	--define "_rpmdir `pwd`" --define "_srcrpmdir `pwd`" \
	-bs openvswitch-ovn-kubernetes.spec
rpm: dist
	rpmbuild --define "_sourcedir `pwd`" --define "_specdir `pwd`" \
	--define "_rpmdir `pwd`" --define "_srcrpmdir `pwd`" \
	-ba openvswitch-ovn-kubernetes.spec

distsnap: openvswitch-ovn-kubernetes.spec.snapshot
	spectool -g openvswitch-ovn-kubernetes.spec.snapshot

rpmsnap: distsnap
	rpmbuild --define "_sourcedir `pwd`" --define "_specdir `pwd`" \
	--define "_rpmdir `pwd`" --define "_srcrpmdir `pwd`" \
	-ba openvswitch-ovn-kubernetes.spec.snapshot

.PHONY: ../go-controller/_output/go/bin/ovnkube

../go-controller/_output/go/bin/ovnkube:
	cd ../go-controller ; make

container: ../go-controller/_output/go/bin/ovnkube
	cp ../go-controller/_output/go/bin/* images

.PHONY: clean
clean:
	-rm -rf *~ \#* .#*

