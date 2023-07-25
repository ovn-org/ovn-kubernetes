package gateway_info

import (
	"fmt"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
)

// GatewayInfoList stores a list of GatewayInfo with unique ips.
// never change GatewayInfoList.elems directly, use GatewayInfoList methods instead
type GatewayInfoList struct {
	elems []*GatewayInfo
}

func NewGatewayInfoList(elems ...*GatewayInfo) *GatewayInfoList {
	gil := &GatewayInfoList{elems: []*GatewayInfo{}}
	gil.InsertOverwrite(elems...)
	return gil
}

func (g *GatewayInfoList) Elems() []*GatewayInfo {
	return g.elems
}

func (g *GatewayInfoList) String() string {
	ret := []string{}
	for _, i := range g.elems {
		ret = append(ret, i.String())
	}
	return strings.Join(ret, ", ")
}

func (g *GatewayInfoList) Has(gw *GatewayInfo) bool {
	for _, i := range g.elems {
		if i.SameSpec(gw) {
			return true
		}
	}
	return false
}

func (g *GatewayInfoList) HasWithoutErr(gw *GatewayInfo) bool {
	for _, i := range g.elems {
		if i.SameSpec(gw) && i.applied {
			return true
		}
	}
	return false
}

// InsertOverwrite should be used to add elements to the GatewayInfoList.
// The latest added gateway with duplicate ip will cause existing gw ip to be deleted.
// This way, we always have only 1 GatewayInfo for every ip.
func (g *GatewayInfoList) InsertOverwrite(gws ...*GatewayInfo) {
	for _, gw := range gws {
		g.insertOverwrite(gw)
	}
}

func (g *GatewayInfoList) insertOverwrite(gw *GatewayInfo) {
	if len(gw.Gateways) == 0 {
		return
	}
	emptyIdxs := []int{}
	for idx, existingGW := range g.elems {
		if existingGW.Equal(gw) {
			// gw already exists in the GatewayInfoList
			return
		}
		// make sure duplicate ips only exist in the latest-added GatewayInfo
		existingGW.RemoveIPs(gw)
		if len(existingGW.Gateways) == 0 {
			// all existingGW ips are overwritten by gw
			emptyIdxs = append(emptyIdxs, idx)
		}
	}

	g.remove(emptyIdxs...)
	g.elems = append(g.elems, gw)
}

func (g *GatewayInfoList) InsertOverwriteFailed(gws ...*GatewayInfo) {
	for _, gw := range gws {
		gw.applied = false
	}
	g.InsertOverwrite(gws...)
}

// Delete removes gatewayInfos that match for all fields, including applied status
func (g *GatewayInfoList) Delete(gws ...*GatewayInfo) {
	elems := make([]*GatewayInfo, 0, len(g.elems))
	for _, i := range g.elems {
		removed := false
		for _, gw := range gws {
			if i.Equal(gw) {
				removed = true
				break
			}
		}
		if !removed {
			elems = append(elems, i)
		}
	}

	g.elems = elems
}

func (g *GatewayInfoList) remove(idxs ...int) {
	if len(idxs) == 0 {
		return
	}
	newElems := make([]*GatewayInfo, 0, len(g.elems))
	idxToDelete := sets.New[int](idxs...)
	for existingIdx, existingElem := range g.elems {
		if !idxToDelete.Has(existingIdx) {
			newElems = append(newElems, existingElem)
		}
	}
	g.elems = newElems
}

func (g *GatewayInfoList) Len() int {
	return len(g.elems)
}

// Equal compares GatewayInfoList elements to be exactly the same, including applied status,
// but ignores elements order.
func (g *GatewayInfoList) Equal(g2 *GatewayInfoList) bool {
	if len(g.elems) != len(g2.elems) {
		return false
	}
	for _, e1 := range g.elems {
		// since GatewayInfoList shouldn't have elements with the same ips, it is safe to assume
		// every element can be equal to only element of another GatewayInfoList
		found := false
		for _, e2 := range g2.elems {
			if e1.Equal(e2) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

type GatewayInfo struct {
	Gateways   sets.Set[string]
	BFDEnabled bool
	applied    bool
}

func (g *GatewayInfo) String() string {
	return fmt.Sprintf("BFDEnabled: %t, Gateways: %+v", g.BFDEnabled, g.Gateways)
}

func NewGatewayInfo(items sets.Set[string], bfdEnabled bool) *GatewayInfo {
	return &GatewayInfo{Gateways: items, BFDEnabled: bfdEnabled}
}

// SameSpec compares GatewayInfo fields, excluding applied
func (g *GatewayInfo) SameSpec(g2 *GatewayInfo) bool {
	return g.BFDEnabled == g2.BFDEnabled && g.Gateways.Equal(g2.Gateways)
}

func (g *GatewayInfo) RemoveIPs(g2 *GatewayInfo) {
	g.Gateways = g.Gateways.Difference(g2.Gateways)
}

// Equal compares all GatewayInfo fields, including BFDEnabled and applied
func (g *GatewayInfo) Equal(g2 *GatewayInfo) bool {
	return g.BFDEnabled == g2.BFDEnabled && g.Gateways.Equal(g2.Gateways) && g.applied == g2.applied
}

func (g *GatewayInfo) Has(ip string) bool {
	return g.Gateways.Has(ip)
}

func (g GatewayInfo) Len() int {
	return g.Gateways.Len()
}
