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

// isNamedUUID checks if the passed id is a named-uuid built with
// BuildNamedUUID
func isNamedUUID(id string) bool {
	return id != "" && id[0] == namedUUIDPrefix
}

// buildNamedUUID builds an id that can be used as a named-uuid
// as per OVSDB rfc 7047 section 5.1
func buildNamedUUID() string {
	return fmt.Sprintf("%c%010d", namedUUIDPrefix, atomic.AddUint32(&namedUUIDCounter, 1))
}
