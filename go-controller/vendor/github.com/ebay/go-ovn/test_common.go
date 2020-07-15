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
	"crypto/x509"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
)

const (
	OVS_RUNDIR           = "/var/run/openvswitch"
	OVNNB_SOCKET         = "nb1.ovsdb"
	OVNSB_SOCKET         = "sb1.ovsdb"
	LR                   = "TEST_LR"
	LRP                  = "TEST_LRP"
	LSW                  = "TEST_LSW"
	LSP                  = "TEST_LSP"
	LSP_SECOND           = "TEST_LSP_SECOND "
	ADDR                 = "36:46:56:76:86:96 127.0.0.1"
	MATCH                = "outport == \"96d44061-1823-428b-a7ce-f473d10eb3d0\" && ip && ip.dst == 10.97.183.61"
	MATCH_SECOND         = "outport == \"96d44061-1823-428b-a7ce-f473d10eb3d0\" && ip && ip.dst == 10.97.183.62"
	defaultClientCACert  = "/etc/openvswitch/client_ca_cert.pem"
	defaultClientPrivKey = "/etc/openvswitch/ovnnb-privkey.pem"
	SKIP_TLS_VERIFY      = true
	SSL                  = "ssl"
	UNIX                 = "unix"
	FAKENOCHASSIS        = "fakenochassis"
	FAKENOSWITCH         = "fakenoswitch"
	FAKENOROUTER         = "fakenorouter"
)

var (
	ovn_db     string
	ovn_socket string
)

func getOVNClient(db string) (ovndbapi Client) {
	var api Client
	var err error
	cfg := &Config{}
	if db == DBNB || db == "" {
		ovn_db = os.Getenv("OVN_NB_DB")
		ovn_socket = OVNNB_SOCKET
	} else {
		ovn_db = os.Getenv("OVN_SB_DB")
		ovn_socket = OVNSB_SOCKET
	}

	cfg.Db = db
	var ovs_rundir = os.Getenv("OVS_RUNDIR")
	if ovs_rundir == "" {
		ovs_rundir = OVS_RUNDIR
	}

	if ovn_db == "" {
		cfg.Addr = UNIX + ":" + ovs_rundir + "/" + ovn_socket

		api, err = NewClient(cfg)
		if err != nil {
			log.Fatal(err)
		}
	} else {
		strs := strings.Split(ovn_db, ":")
		fmt.Println(strs)
		if len(strs) < 2 || len(strs) > 3 {
			log.Fatal("Unexpected format of $OVN_NB/SB_DB")
		}
		if len(strs) == 2 {
			cfg.Addr = UNIX + ":" + ovs_rundir + "/" + strs[1]
			api, err = NewClient(cfg)
			if err != nil {
				log.Fatal(err)
			}
		} else {
			port, _ := strconv.Atoi(strs[2])
			protocol := strs[0]
			if protocol == SSL {
				clientCACert := os.Getenv("CLIENT_CERT_CA_CERT")
				if clientCACert == "" {
					clientCACert = defaultClientCACert
				}
				clientPrivKey := os.Getenv("CLIENT_PRIVKEY")
				if clientPrivKey == "" {
					clientPrivKey = defaultClientPrivKey
				}
				cert, err := tls.LoadX509KeyPair(clientCACert, clientPrivKey)
				if err != nil {
					log.Fatalf("client: loadkeys: %s", err)
				}
				if len(cert.Certificate) != 2 {
					log.Fatal("client.crt should have 2 concatenated certificates: client + CA")
				}
				ca, err := x509.ParseCertificate(cert.Certificate[1])
				if err != nil {
					log.Fatal(err)
				}
				certPool := x509.NewCertPool()
				certPool.AddCert(ca)
				tlsConfig := tls.Config{
					Certificates:       []tls.Certificate{cert},
					RootCAs:            certPool,
					InsecureSkipVerify: SKIP_TLS_VERIFY,
				}
				cfg.TLSConfig = &tlsConfig
			}

			cfg.Addr = fmt.Sprintf("%s:%s:%d", strs[0], strs[1], port)
			api, err = NewClient(cfg)
			if err != nil {
				log.Fatal(err)
			}
		}
	}
	return api
}
