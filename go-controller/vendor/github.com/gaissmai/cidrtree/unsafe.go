package cidrtree

import (
	"hash/crc32"
	"net/netip"
	"unsafe"
)

const sizeOfPrefix = unsafe.Sizeof(netip.Prefix{})

// Use a fast crc32 hash of the key as random number for heap ordering,
// no need to store the prio in everey node.
// The hash must not be calculated for lookups, only during insert, delete and union.
var crc32table = crc32.MakeTable(crc32.Castagnoli)

// prio, calculate the nodes heap priority from the cidr.
// The binary search tree is a treap.
func (n *node) prio() uint32 {
	// safe but MarshalBinary allocates!
	// data, _ := n.cidr.MarshalBinary()

	data := (*[sizeOfPrefix]byte)(unsafe.Pointer(&(n.cidr)))[:]
	return crc32.Checksum(data, crc32table)
}
