[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](http://www.apache.org/licenses/LICENSE-2.0)
[![Go Report Card](https://goreportcard.com/badge/github.com/k8snetworkplumbingwg/sriovnet)](https://goreportcard.com/report/github.com/k8snetworkplumbingwg/sriovnet)
[![Build](https://github.com/k8snetworkplumbingwg/sriovnet/actions/workflows/build.yaml/badge.svg)](https://github.com/k8snetworkplumbingwg/sriovnet/actions/workflows/build.yaml)
[![Test](https://github.com/k8snetworkplumbingwg/sriovnet/actions/workflows/test.yaml/badge.svg)](https://github.com/k8snetworkplumbingwg/sriovnet/actions/workflows/test.yaml)
[![Coverage Status](https://coveralls.io/repos/github/k8snetworkplumbingwg/sriovnet/badge.svg)](https://coveralls.io/k8snetworkplumbingwg/sriovnet)

# sriovnet
Go library to configure SRIOV networking devices

Local build and test

You can use go get command:
```
go get github.com/k8snetworkplumbingwg/sriovnet
```

Example:

```go
package main

import (
    "fmt"

    "github.com/k8snetworkplumbingwg/sriovnet"
)

func main() {
	var vfList[10] *sriovnet.VfObj

	err1 := sriovnet.EnableSriov("ib0")
	if err1 != nil {
		return
	}

	handle, err2 := sriovnet.GetPfNetdevHandle("ib0")
	if err2 != nil {
		return
	}
	err3 := sriovnet.ConfigVfs(handle, false)
	if err3 != nil {
		return
	}
	for i := 0; i < 10; i++ {
		vfList[i], _ = sriovnet.AllocateVf(handle)
	}
	for _, vf := range handle.List {
		fmt.Printf("after allocation vf = %v\n", vf)
	}
	for i := 0; i < 10; i++ {
		if vfList[i] == nil {
			continue
		}
		sriovnet.FreeVf(handle, vfList[i])
	}
	for _, vf := range handle.List {
		fmt.Printf("after free vf = %v\n", vf)
	}
}
```
