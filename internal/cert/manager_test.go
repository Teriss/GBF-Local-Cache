package cert

import (
	"crypto/x509"
	"testing"
)

func TestManagerCreatesRootAndLeafCertificate(t *testing.T) {
	manager := New(t.TempDir())
	if err := manager.Ensure(); err != nil {
		t.Fatal(err)
	}
	status := manager.Status()
	if !status.Ready || status.FingerprintSHA256 == "" {
		t.Fatalf("status = %#v", status)
	}
	certificate, err := manager.CertificateFor("PRD-GAME-A-GRANBLUEFANTASY.AKAMAIZED.NET")
	if err != nil {
		t.Fatal(err)
	}
	if certificate.Leaf == nil || len(certificate.Leaf.DNSNames) != 1 || certificate.Leaf.DNSNames[0] != "prd-game-a-granbluefantasy.akamaized.net" {
		t.Fatalf("leaf SANs = %#v", certificate.Leaf.DNSNames)
	}
	if err := certificate.Leaf.VerifyHostname("prd-game-a-granbluefantasy.akamaized.net"); err != nil {
		t.Fatal(err)
	}
	if _, err := x509.ParseCertificate(certificate.Certificate[1]); err != nil {
		t.Fatal(err)
	}
}
