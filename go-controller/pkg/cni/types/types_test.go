// SPDX-FileCopyrightText: Copyright The OVN-Kubernetes Contributors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"testing"
)

func TestNetConfMarshalJSONIncludesNoOverlayFields(t *testing.T) {
	netConf := NetConf{
		Transport:        "no-overlay",
		OutboundSNAT:     "enabled",
		NoOverlayRouting: "unmanaged",
	}

	rawNetConf, err := json.Marshal(netConf)
	if err != nil {
		t.Fatalf("failed to marshal NetConf: %v", err)
	}

	var parsedNetConf struct {
		Transport        string `json:"transport"`
		OutboundSNAT     string `json:"outboundSNAT"`
		NoOverlayRouting string `json:"noOverlayRouting"`
	}
	if err := json.Unmarshal(rawNetConf, &parsedNetConf); err != nil {
		t.Fatalf("failed to unmarshal NetConf: %v", err)
	}

	if parsedNetConf.Transport != netConf.Transport {
		t.Fatalf("expected transport %q, got %q", netConf.Transport, parsedNetConf.Transport)
	}
	if parsedNetConf.OutboundSNAT != netConf.OutboundSNAT {
		t.Fatalf("expected outboundSNAT %q, got %q", netConf.OutboundSNAT, parsedNetConf.OutboundSNAT)
	}
	if parsedNetConf.NoOverlayRouting != netConf.NoOverlayRouting {
		t.Fatalf("expected noOverlayRouting %q, got %q", netConf.NoOverlayRouting, parsedNetConf.NoOverlayRouting)
	}
}
