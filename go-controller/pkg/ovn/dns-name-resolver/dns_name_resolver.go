package dnsnameresolver

import (
	"time"

	addressset "github.com/ovn-org/ovn-kubernetes/go-controller/pkg/ovn/address_set"
)

type DNSNameResolver interface {
	Add(AddRequest) AddResponse
	Delete(DeleteRequest) DeleteResponse
	Update(UpdateRequest) UpdateResponse
	AddNamespace(AddNamespaceRequest) AddNamespaceResponse
	RemoveNamespace(RemoveNamespaceRequest) RemoveNamespaceResponse
	GetAddressSet(GetAddressSetRequest) GetAddressSetResponse
	Run(RunRequest)
	Shutdown()
}

type AddRequest struct {
	Namespace string
	DNSName   string
	Addresses []string
}

type AddResponse struct {
	DNSAddressSet addressset.AddressSet
	Err           error
}

type DeleteRequest struct {
	Namespace string
	DNSName   string
}

type DeleteResponse struct {
	Err error
}

type UpdateRequest struct {
	DNSName string
}

type UpdateResponse struct {
	IsUpdated bool
	Err       error
}

type AddNamespaceRequest struct {
	Namespace string
	DNSName   string
}

type AddNamespaceResponse struct {
	Err error
}

type RemoveNamespaceRequest struct {
	Namespace string
}

type RemoveNamespaceResponse struct {
	Err error
}

type GetAddressSetRequest struct {
	DNSName string
}

type GetAddressSetResponse struct {
	DNSAddressSet addressset.AddressSet
	Err           error
}

type RunRequest struct {
	DefaultInterval time.Duration
}
