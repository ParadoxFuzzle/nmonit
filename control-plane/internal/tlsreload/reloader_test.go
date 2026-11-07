package tlsreload

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeCert(t *testing.T, dir string, serial int64) (certPath, keyPath string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(serial),
		Subject:               pkix.Name{CommonName: "test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create certificate: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}

	certPath = filepath.Join(dir, "server.crt")
	keyPath = filepath.Join(dir, "server.key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatalf("write certificate: %v", err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
	return certPath, keyPath
}

func serial(t *testing.T, r *CertReloader) int64 {
	t.Helper()

	cert, err := r.GetCertificate(nil)
	if err != nil {
		t.Fatalf("get certificate: %v", err)
	}
	parsed, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		t.Fatalf("parse certificate: %v", err)
	}
	return parsed.SerialNumber.Int64()
}

func TestReloadForcesUnchangedModTimes(t *testing.T) {
	dir := t.TempDir()
	certPath, keyPath := writeCert(t, dir, 1)

	reloader, err := New(certPath, keyPath, "")
	if err != nil {
		t.Fatalf("new reloader: %v", err)
	}
	if got := serial(t, reloader); got != 1 {
		t.Fatalf("initial serial = %d, want 1", got)
	}

	certInfo, err := os.Stat(certPath)
	if err != nil {
		t.Fatalf("stat certificate: %v", err)
	}
	keyInfo, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("stat key: %v", err)
	}
	certMod := certInfo.ModTime()
	keyMod := keyInfo.ModTime()

	writeCert(t, dir, 2)
	if err := os.Chtimes(certPath, certMod, certMod); err != nil {
		t.Fatalf("restore certificate mtime: %v", err)
	}
	if err := os.Chtimes(keyPath, keyMod, keyMod); err != nil {
		t.Fatalf("restore key mtime: %v", err)
	}

	if err := reloader.load(false); err != nil {
		t.Fatalf("poll reload: %v", err)
	}
	if got := serial(t, reloader); got != 1 {
		t.Fatalf("poll reload changed cert with unchanged mtimes: serial = %d", got)
	}

	reloader.Reload()
	if got := serial(t, reloader); got != 2 {
		t.Fatalf("forced reload serial = %d, want 2", got)
	}
}
