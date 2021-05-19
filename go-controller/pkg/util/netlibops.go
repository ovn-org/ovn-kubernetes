package util

import (
	"net"
)

type NETOps interface {
	InterfaceAddrs() ([]net.Addr, error)
}

type defaultNETOps struct{}

var netOps NETOps = &defaultNETOps{}

func SetNETLibOpsMockInst(mockInst NETOps) {
	netOps = mockInst
}

func GetNETLibOps() NETOps {
	return netOps
}

func (defaultNETOps) InterfaceAddrs() ([]net.Addr, error) {
	return net.InterfaceAddrs()
}
