module github.com/ovn-org/ovn-kubernetes/go-controller

go 1.21

require (
	github.com/Microsoft/hcsshim v0.9.6
	github.com/alexflint/go-filemutex v1.2.0
	github.com/asaskevich/govalidator v0.0.0-20210307081110-f21760c49a8d
	github.com/bhendo/go-powershell v0.0.0-20190719160123-219e7fb4e41e
	github.com/cenkalti/backoff/v4 v4.2.1
	github.com/containernetworking/cni v1.1.2
	github.com/containernetworking/plugins v1.2.0
	github.com/coreos/go-iptables v0.6.0
	github.com/fsnotify/fsnotify v1.7.0
	github.com/gaissmai/cidrtree v0.1.4
	github.com/go-logr/logr v1.4.1
	github.com/go-logr/stdr v1.2.2
	github.com/google/go-cmp v0.6.0
	github.com/google/uuid v1.6.0
	github.com/gorilla/mux v1.8.0
	github.com/j-keck/arping v1.0.2
	github.com/k8snetworkplumbingwg/govdpa v0.1.5-0.20230926073613-07c1031aea47
	github.com/k8snetworkplumbingwg/ipamclaims v0.4.0-alpha
	github.com/k8snetworkplumbingwg/multi-networkpolicy v0.0.0-20200914073308-0f33b9190170
	github.com/k8snetworkplumbingwg/network-attachment-definition-client v1.6.0
	github.com/k8snetworkplumbingwg/sriovnet v1.2.1-0.20230427090635-4929697df2dc
	github.com/mdlayher/ndp v1.0.1
	github.com/miekg/dns v1.1.31
	github.com/mitchellh/copystructure v1.2.0
	github.com/onsi/ginkgo v1.16.5
	github.com/onsi/gomega v1.30.0
	github.com/openshift/api v0.0.0-20231120222239-b86761094ee3
	github.com/openshift/client-go v0.0.0-20231121143148-910ca30a1a9a
	github.com/ovn-org/libovsdb v0.6.1-0.20240125124854-03f787b1a892
	github.com/pkg/errors v0.9.1
	github.com/prometheus/client_golang v1.18.0
	github.com/prometheus/client_model v0.5.0
	github.com/safchain/ethtool v0.3.1-0.20231027162144-83e5e0097c91
	github.com/spf13/afero v1.9.5
	github.com/stretchr/testify v1.8.4
	github.com/urfave/cli/v2 v2.2.0
	github.com/vishvananda/netlink v1.2.1-beta.2.0.20231024175852-77df5d35f725
	golang.org/x/exp v0.0.0-20230811145659-89c5cff77bcb
	golang.org/x/net v0.23.0
	golang.org/x/sync v0.6.0
	golang.org/x/sys v0.18.0
	golang.org/x/time v0.3.0
	google.golang.org/grpc v1.62.1
	google.golang.org/grpc/security/advancedtls v0.0.0-20240425232638-1e8b9b7fc655
	google.golang.org/protobuf v1.33.0
	gopkg.in/fsnotify/fsnotify.v1 v1.4.7
	gopkg.in/gcfg.v1 v1.2.3
	gopkg.in/natefinch/lumberjack.v2 v2.2.1
	k8s.io/api v0.29.2
	k8s.io/apimachinery v0.29.2
	k8s.io/client-go v0.29.2
	k8s.io/component-helpers v0.29.2
	k8s.io/klog/v2 v2.110.1
	k8s.io/kubernetes v1.29.2
	k8s.io/utils v0.0.0-20230726121419-3b25d923346b
	kubevirt.io/api v1.0.0-alpha.0
	sigs.k8s.io/controller-runtime v0.17.0
	sigs.k8s.io/network-policy-api v0.1.5
	sigs.k8s.io/structured-merge-diff/v4 v4.4.1
)

require (
	github.com/Microsoft/go-winio v0.6.0 // indirect
	github.com/beorn7/perks v1.0.1 // indirect
	github.com/cenkalti/hub v1.0.1 // indirect
	github.com/cenkalti/rpc2 v0.0.0-20210604223624-c1acbc6ec984 // indirect
	github.com/cespare/xxhash/v2 v2.2.0 // indirect
	github.com/containerd/cgroups v1.1.0 // indirect
	github.com/cpuguy83/go-md2man/v2 v2.0.2 // indirect
	github.com/davecgh/go-spew v1.1.1 // indirect
	github.com/emicklei/go-restful/v3 v3.11.0 // indirect
	github.com/evanphx/json-patch v5.6.0+incompatible // indirect
	github.com/evanphx/json-patch/v5 v5.8.0 // indirect
	github.com/go-openapi/jsonpointer v0.19.6 // indirect
	github.com/go-openapi/jsonreference v0.20.2 // indirect
	github.com/go-openapi/swag v0.22.3 // indirect
	github.com/gogo/protobuf v1.3.2 // indirect
	github.com/golang/groupcache v0.0.0-20210331224755-41bb18bfe9da // indirect
	github.com/golang/protobuf v1.5.3 // indirect
	github.com/google/gnostic-models v0.6.8 // indirect
	github.com/google/gofuzz v1.2.0 // indirect
	github.com/gorilla/websocket v1.5.0 // indirect
	github.com/imdario/mergo v0.3.12 // indirect
	github.com/josharian/intern v1.0.0 // indirect
	github.com/json-iterator/go v1.1.12 // indirect
	github.com/juju/errors v0.0.0-20200330140219-3fe23663418f // indirect
	github.com/juju/testing v0.0.0-20200706033705-4c23f9c453cd // indirect
	github.com/mailru/easyjson v0.7.7 // indirect
	github.com/matttproud/golang_protobuf_extensions/v2 v2.0.0 // indirect
	github.com/mitchellh/reflectwalk v1.0.2 // indirect
	github.com/moby/spdystream v0.2.0 // indirect
	github.com/modern-go/concurrent v0.0.0-20180306012644-bacd9c7ef1dd // indirect
	github.com/modern-go/reflect2 v1.0.2 // indirect
	github.com/munnerz/goautoneg v0.0.0-20191010083416-a7dc8b61c822 // indirect
	github.com/mxk/go-flowrate v0.0.0-20140419014527-cca7078d478f // indirect
	github.com/nxadm/tail v1.4.8 // indirect
	github.com/openshift/custom-resource-status v1.1.2 // indirect
	github.com/pborman/uuid v1.2.0 // indirect
	github.com/pmezard/go-difflib v1.0.0 // indirect
	github.com/prometheus/common v0.45.0 // indirect
	github.com/prometheus/procfs v0.12.0 // indirect
	github.com/russross/blackfriday/v2 v2.1.0 // indirect
	github.com/sirupsen/logrus v1.9.0 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
	github.com/stretchr/objx v0.5.0 // indirect
	github.com/vishvananda/netns v0.0.4 // indirect
	go.opencensus.io v0.24.0 // indirect
	golang.org/x/crypto v0.21.0 // indirect
	golang.org/x/mod v0.14.0 // indirect
	golang.org/x/oauth2 v0.16.0 // indirect
	golang.org/x/term v0.18.0 // indirect
	golang.org/x/text v0.14.0 // indirect
	golang.org/x/tools v0.17.0 // indirect
	gomodules.xyz/jsonpatch/v2 v2.4.0 // indirect
	google.golang.org/appengine v1.6.8 // indirect
	google.golang.org/genproto/googleapis/rpc v0.0.0-20240318140521-94a12d6c2237 // indirect
	gopkg.in/inf.v0 v0.9.1 // indirect
	gopkg.in/tomb.v1 v1.0.0-20141024135613-dd632973f1e7 // indirect
	gopkg.in/warnings.v0 v0.1.2 // indirect
	gopkg.in/yaml.v2 v2.4.0 // indirect
	gopkg.in/yaml.v3 v3.0.1 // indirect
	k8s.io/apiextensions-apiserver v0.29.2 // indirect
	k8s.io/component-base v0.29.2 // indirect
	k8s.io/kube-openapi v0.0.0-20231010175941-2dd684a91f00 // indirect
	kubevirt.io/containerized-data-importer-api v1.55.0 // indirect
	kubevirt.io/controller-lifecycle-operator-sdk/api v0.0.0-20220329064328-f3cc58c6ed90 // indirect
	sigs.k8s.io/json v0.0.0-20221116044647-bc3834ca7abd // indirect
	sigs.k8s.io/yaml v1.4.0 // indirect
)

replace (
	github.com/Microsoft/hcsshim => github.com/Microsoft/hcsshim v0.8.20
	github.com/coreos/go-iptables => github.com/trozet/go-iptables v0.0.0-20240328221912-077e672b3808
	github.com/gogo/protobuf => github.com/gogo/protobuf v1.3.2
	github.com/j-keck/arping => github.com/JacobTanenbaum/arping v0.0.0-20240209152419-3987db83bd51
)
