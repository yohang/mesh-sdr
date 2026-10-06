package infra_test

import (
	"crypto/x509"
	"net"
	"testing"
	"time"

	"github.com/yohang/mesh-sdr/internal/grid/domain"
	"github.com/yohang/mesh-sdr/internal/grid/infra"
)

func TestSelfSignedCertificate(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		listen string
		ip     string
		dns    string
	}{
		{listen: "0.0.0.0:8074"},
		{listen: "192.0.2.10:8074", ip: "192.0.2.10"},
		{listen: "node.example.org:8074", dns: "node.example.org"},
	}

	for _, tt := range tests {
		t.Run(tt.listen, func(t *testing.T) {
			cert, err := infra.SelfSignedCertificate(domain.MustNodeID("attic"), tt.listen, now)
			if err != nil {
				t.Fatal(err)
			}

			leaf, err := x509.ParseCertificate(cert.Certificate[0])
			if err != nil {
				t.Fatal(err)
			}

			if len(leaf.URIs) != 1 || leaf.URIs[0].String() != "urn:rx:node:attic" {
				t.Errorf("URIs = %v", leaf.URIs)
			}

			if !leaf.NotBefore.Before(now) || !leaf.NotAfter.After(now.Add(24*time.Hour)) {
				t.Errorf("validity %v → %v", leaf.NotBefore, leaf.NotAfter)
			}

			if tt.ip != "" && (len(leaf.IPAddresses) != 1 || !leaf.IPAddresses[0].Equal(net.ParseIP(tt.ip))) {
				t.Errorf("IPs = %v", leaf.IPAddresses)
			}

			if tt.dns != "" && (len(leaf.DNSNames) != 1 || leaf.DNSNames[0] != tt.dns) {
				t.Errorf("DNS = %v", leaf.DNSNames)
			}

			if err := leaf.CheckSignature(leaf.SignatureAlgorithm, leaf.RawTBSCertificate, leaf.Signature); err != nil {
				t.Errorf("not self-signed: %v", err)
			}
		})
	}
}
