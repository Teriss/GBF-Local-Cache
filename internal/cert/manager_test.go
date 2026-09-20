package cert

import (
	"crypto/tls"
	"crypto/x509"
	"testing"
	"time"
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

func TestManagerRenewsLeafCertificateNearExpiry(t *testing.T) {
	manager := New(t.TempDir())
	first, err := manager.CertificateFor("static.example.test")
	if err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.leaf["static.example.test"] = tls.Certificate{
		Certificate: first.Certificate,
		PrivateKey:  first.PrivateKey,
		Leaf:        &x509.Certificate{NotAfter: time.Now().Add(30 * time.Minute)},
	}
	manager.mu.Unlock()
	second, err := manager.CertificateFor("static.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Certificate) == 0 || len(second.Certificate) == 0 || string(first.Certificate[0]) == string(second.Certificate[0]) {
		t.Fatal("leaf certificate was not renewed")
	}
	if !second.Leaf.NotAfter.After(time.Now().Add(23 * time.Hour)) {
		t.Fatalf("renewed leaf expires too soon: %s", second.Leaf.NotAfter)
	}
}
