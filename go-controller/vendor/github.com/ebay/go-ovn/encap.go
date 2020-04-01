/**
 * Copyright (c) 2020 eBay Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 **/

package goovn

import (
	"fmt"
	"github.com/ebay/libovsdb"
)

// Encap table OVN SB
type Encap struct {
	UUID        string
	ChassisName string
	Ip          string
	Options     map[interface{}]interface{}
	Encaptype   string
}

func (odbi *ovndb) encapListImp(chassisName string) ([]*Encap, error) {

	odbi.cachemutex.RLock()
	defer odbi.cachemutex.RUnlock()

	cacheChassis, ok := odbi.cache[tableChassis]
	if !ok {
		return nil, ErrorNotFound
	}

	var encaps []*Encap
	var chFound bool
	for _, drows := range cacheChassis {
		if ch, ok := drows.Fields["name"].(string); ok && ch == chassisName {
			if enc, ok := drows.Fields["encaps"]; ok {
				switch enc.(type) {
				case libovsdb.UUID:
					if enuid, ok := enc.(libovsdb.UUID); ok {
						cenc, err := odbi.rowToEncap(enuid.GoUUID)
						if err != nil {
							return nil, err
						}
						encaps = append(encaps, cenc)
					} else {
						return nil, fmt.Errorf("type libovsdb.UUID casting failed")
					}
				case libovsdb.OvsSet:
					if en, ok := enc.(libovsdb.OvsSet); ok {
						for _, e := range en.GoSet {
							if euid, ok := e.(libovsdb.UUID); ok {
								enc, err := odbi.rowToEncap(euid.GoUUID)
								if err != nil {
									return nil, err
								}
								encaps = append(encaps, enc)
							}
						}
					} else {
						return nil, fmt.Errorf("type libovsdb.OvsSet casting failed")
					}
				}
			}
			chFound = true
			break
		}
	}
	if !chFound {
		return nil, ErrorNotFound
	}
	return encaps, nil

}

func (odbi *ovndb) rowToEncap(uuid string) (*Encap, error) {
	cacheEncaps, ok := odbi.cache[tableEncap][uuid]
	if !ok {
		return nil, fmt.Errorf("Encap with uuid%s not found", uuid)
	}
	en := &Encap{
		UUID:        uuid,
		ChassisName: cacheEncaps.Fields["chassis_name"].(string),
		Ip:          cacheEncaps.Fields["ip"].(string),
		Options:     cacheEncaps.Fields["options"].(libovsdb.OvsMap).GoMap,
		Encaptype:   cacheEncaps.Fields["type"].(string),
	}
	return en, nil
}
