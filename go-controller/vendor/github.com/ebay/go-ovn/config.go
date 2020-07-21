/**
 * Copyright (c) 2017 eBay Inc.
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
	"crypto/tls"
)

// Config ovn nb and sb db client config
type Config struct {
	Db           string
	Addr         string
	TLSConfig    *tls.Config
	SignalCB     OVNSignal
	DisconnectCB OVNDisconnectedCallback // Callback that is called when disconnected, if "Reconnect" is false.
	Reconnect    bool                    // Automatically reconnect when disconnected
}
