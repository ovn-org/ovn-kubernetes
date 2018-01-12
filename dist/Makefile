# Copyright (C) 2018 Red Hat Inc.
#
# Copying and distribution of this file, with or without modification,
# are permitted in any medium without royalty provided the copyright
# notice and this notice are preserved.  This file is offered as-is,
# without warranty of any kind.


rpms: srpm rpm

dist: openvswitch-ovn-kubernetes.spec
	spectool -g openvswitch-ovn-kubernetes.spec

srpm: dist
	rpmbuild --define "_sourcedir `pwd`" --define "_specdir `pwd`" \
	--define "_rpmdir `pwd`" --define "_srcrpmdir `pwd`" -bs openvswitch-ovn-kubernetes.spec
rpm: dist
	rpmbuild --define "_sourcedir `pwd`" --define "_specdir `pwd`" \
	--define "_rpmdir `pwd`" --define "_srcrpmdir `pwd`" -ba openvswitch-ovn-kubernetes.spec

.PHONY: clean
clean:
	-rm -rf *~ \#* .#*

