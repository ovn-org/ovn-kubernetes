%global project ovn-kubernetes
%global repo %{project}
%global debug_package %{nil}

# some distros (e.g: RHEL-7) don't define _rundir macro yet
# Fedora 15 onwards uses /run as _rundir
%if 0%{!?_rundir:1}
%define _rundir /run
%endif

# define the python package prefix based on distribution version so that we can
# simultaneously support RHEL-based and later Fedora versions in this spec file.
%if 0%{?fedora} >= 25
%define _py2 python2
%endif

%if 0%{?rhel} || 0%{?fedora} < 25
%define _py2 python
%endif

Name: openvswitch-ovn-kubernetes
Version: 0.3.0
Release: 1%{?dist}
URL: https://www.github.com/openvswitch/ovn-kubernetes
Summary: Open Virtual Networking Kubernetes Wedge

License: ASL 2.0
Source0: https://github.com/openvswitch/ovn-kubernetes/archive/v%{version}.tar.gz

# golang not supported
ExcludeArch: ppc64

BuildRequires: %{_py2}-devel
%if 0%{?fedora} > 22 || %{with build_python3}
BuildRequires: python3-devel
%endif
BuildRequires: golang

%description
This allows kubernetes to use Open Virtual Networking (OVN)

%package master
Summary: ovn-kubernetes systemd for master
License: ASL 2.0
#Requires: openvswitch-ovn-kubernetes systemd openvswitch

%description master
This allows systemd to control ovn on the master

%package node
Summary: ovn-kubernetes systemd for node
License: ASL 2.0
#Requires: openvswitch-ovn-kubernetes systemd openvswitch

%description node
This allows systemd to control ovn on the node

%prep
%setup -q -n %{repo}-%{version}

%build
cd go-controller && make
strip _output/go/bin/ovnkube
strip _output/go/bin/ovn-kube-util
strip _output/go/bin/ovn-k8s-overlay
strip _output/go/bin/ovn-k8s-cni-overlay

%install
install -d -m 0750 %{buildroot}%{_bindir}
install -d -m 0750 %{buildroot}%{_libexecdir}/cni
install -p -m 755 go-controller/_output/go/bin/ovnkube %{buildroot}%{_bindir}
install -p -m 755 go-controller/_output/go/bin/ovn-kube-util %{buildroot}%{_bindir}
install -p -m 755 go-controller/_output/go/bin/ovn-k8s-overlay %{buildroot}%{_bindir}
install -p -m 755 go-controller/_output/go/bin/ovn-k8s-cni-overlay %{buildroot}%{_libexecdir}/cni
install -d -m 0750 %{buildroot}/etc/openvswitch
install -p -m 644 go-controller/etc/ovn_k8s.conf %{buildroot}/etc/openvswitch
install -d -m 0750 %{buildroot}%{_mandir}/man1
install -p -m 644 docs/ovnkube.1 %{buildroot}%{_mandir}/man1
install -p -m 644 docs/ovn-kube-util.1 %{buildroot}%{_mandir}/man1
install -p -m 644 docs/ovn-k8s-overlay.1 %{buildroot}%{_mandir}/man1
install -d -m 0750 %{buildroot}%{_mandir}/man5
install -p -m 644 docs/ovn_k8s.conf.5 %{buildroot}%{_mandir}/man5

install -p -D -m 0644 dist/files/ovn-kubernetes-master.service \
                   %{buildroot}%{_unitdir}/ovn-kubernetes-master.service
install -p -D -m 0644 dist/files/ovn-kubernetes-node.service \
                   %{buildroot}%{_unitdir}/ovn-kubernetes-node.service
install -p -m 755 dist/files/ovn-kubernetes-master.sh %{buildroot}%{_bindir}
install -p -m 755 dist/files/ovn-kubernetes-node.sh %{buildroot}%{_bindir}
install -p -D -m 0644 dist/files/ovn-kubernetes.sysconfig \
                   %{buildroot}%{_sysconfdir}/sysconfig/ovn-kubernetes

%preun node
    %systemd_preun openvswitch-ovn-kubernetes-node

%preun master
    %systemd_preun openvswitch-ovn-kubernetes-master

%post node
    %systemd_post openvswitch-ovn-kubernetes-node

%post master
    %systemd_post openvswitch-ovn-kubernetes-master


%files
%defattr(-,root,root)
%license COPYING
%doc CONTRIBUTING.md README.md
%doc docs/config.md  docs/debugging.md  docs/INSTALL.SSL.md  docs/INSTALL.UBUNTU.md
%{_mandir}/man1/ovnkube.1.*
%{_mandir}/man1/ovn-kube-util.1.*
%{_mandir}/man1/ovn-k8s-overlay.1.*
%{_mandir}/man5/ovn_k8s.conf.5.*
%{_bindir}/ovnkube
%{_bindir}/ovn-kube-util
%{_bindir}/ovn-k8s-overlay
%{_libexecdir}/cni/ovn-k8s-cni-overlay
%config(noreplace) %{_sysconfdir}/openvswitch/ovn_k8s.conf

%files node
%{_unitdir}/ovn-kubernetes-node.service
%{_bindir}/ovn-kubernetes-node.sh
%config(noreplace) %{_sysconfdir}/sysconfig/ovn-kubernetes

%files master
%{_unitdir}/ovn-kubernetes-master.service
%{_bindir}/ovn-kubernetes-master.sh


%changelog
* Wed May 9 2018 Phil Cameron <pcameron@redhat.com> - 0.3.0-1
- Added support for containers

* Fri Mar 23 2018 Phil Cameron <pcameron@redhat.com> - 0.2.0-1
- Added packages for systemd packages openvswitch-ovn-kubernetes-node
  and openvswitch-ovn-kubernetes-master.

* Thu Jan 25 2018 Phil Cameron <pcameron@redhat.com> - 0.1.0-2
- Changed from referencing a commit to referencing a release
  in the source repo.

* Fri Jan 12 2018 Phil Cameron <pcameron@redhat.com> - 0.1.0-1
- Initial package for Fedora

