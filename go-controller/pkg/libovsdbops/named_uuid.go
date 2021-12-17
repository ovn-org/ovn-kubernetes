package libovsdbops

import (
	"fmt"
	"math/rand"
	"sync/atomic"
)

const (
	namedUUIDPrefix = 'u'
)

var (
	namedUUIDCounter = rand.Uint32()
)

// IsNamedUUID checks if the passed id is a named-uuid built with
// BuildNamedUUID
func IsNamedUUID(id string) bool {
	return id != "" && id[0] == namedUUIDPrefix
}

// BuildNamedUUID builds an id that can be used as a named-uuid
// as per OVSDB rfc 7047 section 5.1
func BuildNamedUUID() string {
	return fmt.Sprintf("%c%010d", namedUUIDPrefix, atomic.AddUint32(&namedUUIDCounter, 1))
}
