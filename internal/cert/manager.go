package cert

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"gbf-local-cache/internal/host"
	"gbf-local-cache/internal/platform"
)

type Status struct {
	Ready             bool      `json:"ready"`
	Installed         bool      `json:"installed"`
	FingerprintSHA256 string    `json:"fingerprint_sha256,omitempty"`
	CreatedAt         time.Time `json:"created_at,omitempty"`
	Directory         string    `json:"directory"`
	Error             string    `json:"error,omitempty"`
}

type Manager struct {
	mu           sync.Mutex
	directory    string
	caCert       *x509.Certificate
	caKey        *ecdsa.PrivateKey
	createdAt    time.Time
	leaf         map[string]tls.Certificate
	installed    bool
	installKnown bool
}

const leafRenewalWindow = time.Hour

func DefaultDirectory() (string, error) {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		var err error
		base, err = os.UserConfigDir()
		if err != nil {
			return "", err
		}
	}
	return filepath.Join(base, "GBFLocalCache", "certs"), nil
}

func New(directory string) *Manager {
	return &Manager{directory: directory, leaf: make(map[string]tls.Certificate)}
}

func (m *Manager) Ensure() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.caCert != nil && m.caKey != nil {
		return nil
	}
	if err := os.MkdirAll(m.directory, 0o700); err != nil {
		return fmt.Errorf("create certificate directory: %w", err)
	}
	certPEM, certErr := os.ReadFile(filepath.Join(m.directory, "ca.crt"))
	keyPEM, keyErr := os.ReadFile(filepath.Join(m.directory, "ca.key"))
	if certErr == nil && keyErr == nil {
		cert, key, createdAt, err := parseCA(certPEM, keyPEM)
		if err == nil {
			m.caCert, m.caKey, m.createdAt = cert, key, createdAt
			return nil
		}
	}
	return m.generateLocked()
}

func (m *Manager) generateLocked() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	serial, err := randomSerial()
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "GBF Local Cache Root CA"},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            1,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := atomicWrite(filepath.Join(m.directory, "ca.crt"), certPEM, 0o644); err != nil {
		return err
	}
	if err := atomicWrite(filepath.Join(m.directory, "ca.key"), keyPEM, 0o600); err != nil {
		return err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return err
	}
	m.caCert, m.caKey, m.createdAt = cert, key, now
	m.leaf = make(map[string]tls.Certificate)
	return nil
}

func (m *Manager) CertificateFor(hostname string) (tls.Certificate, error) {
	normalized, ok := host.Normalize(hostname)
	if !ok {
		return tls.Certificate{}, fmt.Errorf("certificate host is not a valid DNS name")
	}
	hostname = normalized
	if err := m.Ensure(); err != nil {
		return tls.Certificate{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if certificate, ok := m.leaf[hostname]; ok {
		if certificate.Leaf != nil && time.Now().Before(certificate.Leaf.NotAfter.Add(-leafRenewalWindow)) {
			return certificate, nil
		}
	}
	serial, err := randomSerial()
	if err != nil {
		return tls.Certificate{}, err
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now().UTC()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: hostname},
		DNSNames:     []string{hostname},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, m.caCert, &key.PublicKey, m.caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	certificate := tls.Certificate{Certificate: [][]byte{der, m.caCert.Raw}, PrivateKey: key, Leaf: template}
	m.leaf[hostname] = certificate
	return certificate, nil
}

func (m *Manager) Status() Status {
	status := Status{Directory: m.directory}
	if err := m.Ensure(); err != nil {
		status.Error = err.Error()
		return status
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	status.Ready = m.caCert != nil && m.caKey != nil
	status.FingerprintSHA256 = fingerprint(m.caCert.Raw, sha256.New)
	status.CreatedAt = m.createdAt
	if !m.installKnown {
		m.installed = isInstalled(m.caCert)
		m.installKnown = true
	}
	status.Installed = m.installed
	return status
}

func (m *Manager) Install() error {
	if err := m.Ensure(); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return errors.New("certificate installation is only supported on Windows")
	}
	path := filepath.Join(m.directory, "ca.crt")
	if output, err := runCertificateCommand("certutil.exe", "-addstore", "-user", "Root", path); err != nil {
		return fmt.Errorf("install Root CA: %w: %s", err, strings.TrimSpace(string(output)))
	}
	m.mu.Lock()
	m.installed = true
	m.installKnown = true
	m.mu.Unlock()
	return nil
}

func (m *Manager) Uninstall() error {
	if err := m.Ensure(); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return errors.New("certificate removal is only supported on Windows")
	}
	thumbprint := fingerprint(m.caCert.Raw, sha1.New)
	if output, err := runCertificateCommand("certutil.exe", "-delstore", "-user", "Root", thumbprint); err != nil {
		return fmt.Errorf("remove Root CA: %w: %s", err, strings.TrimSpace(string(output)))
	}
	m.mu.Lock()
	m.installed = false
	m.installKnown = true
	m.mu.Unlock()
	return nil
}

func (m *Manager) Regenerate() error {
	if err := m.Ensure(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	oldCertificate := m.caCert
	oldInstalled := false
	if oldCertificate != nil && runtime.GOOS == "windows" {
		// Regeneration must inspect the store instead of trusting the cached
		// flag: the user may have installed or removed the CA outside this app.
		m.installed = isInstalled(oldCertificate)
		m.installKnown = true
		oldInstalled = m.installed
	}
	oldThumbprint := ""
	if oldCertificate != nil {
		oldThumbprint = fingerprint(oldCertificate.Raw, sha1.New)
	}
	if oldInstalled && runtime.GOOS == "windows" {
		if output, err := runCertificateCommand("certutil.exe", "-delstore", "-user", "Root", oldThumbprint); err != nil {
			return fmt.Errorf("remove previous Root CA: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}
	if err := os.Remove(filepath.Join(m.directory, "ca.crt")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(filepath.Join(m.directory, "ca.key")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	m.caCert, m.caKey, m.createdAt = nil, nil, time.Time{}
	m.leaf = make(map[string]tls.Certificate)
	m.installed = false
	m.installKnown = true
	if err := m.generateLocked(); err != nil {
		return err
	}
	if oldInstalled && runtime.GOOS == "windows" {
		path := filepath.Join(m.directory, "ca.crt")
		if output, err := runCertificateCommand("certutil.exe", "-addstore", "-user", "Root", path); err != nil {
			return fmt.Errorf("install regenerated Root CA: %w: %s", err, strings.TrimSpace(string(output)))
		}
		m.installed = true
		m.installKnown = true
	}
	return nil
}

func parseCA(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, time.Time, error) {
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, nil, time.Time{}, errors.New("certificate PEM is invalid")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, nil, time.Time{}, err
	}
	createdAt := cert.NotBefore.Add(5 * time.Minute)
	return cert, key, createdAt, nil
}

func randomSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	return serial, nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cert-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return platform.ReplaceFile(tmpName, path)
}

func fingerprint(data []byte, newHash func() hash.Hash) string {
	digest := newHash()
	_, _ = digest.Write(data)
	encoded := strings.ToUpper(hex.EncodeToString(digest.Sum(nil)))
	parts := make([]string, 0, len(encoded)/2)
	for index := 0; index < len(encoded); index += 2 {
		parts = append(parts, encoded[index:index+2])
	}
	return strings.Join(parts, ":")
}

func isInstalled(certificate *x509.Certificate) bool {
	if certificate == nil || runtime.GOOS != "windows" {
		return false
	}
	thumbprint := strings.ReplaceAll(fingerprint(certificate.Raw, sha1.New), ":", "")
	output, err := runCertificateCommand("certutil.exe", "-user", "-store", "Root")
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToUpper(string(output)), strings.ToUpper(thumbprint))
}
