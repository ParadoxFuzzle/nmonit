// Package tlsreload provides hot-reloadable TLS certificate management.
//
// CertReloader loads a certificate + key pair (and optional CA certificate for
// mTLS) from disk and periodically watches for changes.  It exposes
// GetCertificate and GetConfigForClient callbacks that can be plugged into
// both net/http and google.golang.org/grpc so that new TLS connections
// automatically use the updated certificate without downtime.
package tlsreload

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/compute-nmonit/control-plane/internal/metrics"
	"github.com/rs/zerolog/log"
)

// CertReloader loads and hot-reloads TLS certificates from disk.
//
// After initial creation, call Watch() in a background goroutine to poll
// for file changes.  The GetCertificate and GetConfigForClient methods
// are safe for concurrent use and always return the most recently loaded
// certificate.
type CertReloader struct {
	mu sync.RWMutex

	cert         *tls.Certificate
	clientCAPool *x509.CertPool
	hasCA        bool

	certPath   string
	keyPath    string
	caCertPath string

	// Tracked modification times for polling change detection.
	certMod time.Time
	keyMod  time.Time
	caMod   time.Time
}

// New creates a CertReloader, loading the certificate from disk immediately.
// If loading fails, an error is returned and the caller should treat it as
// a fatal startup error.
//
// certPath and keyPath are required.  caCertPath may be empty (TLS only,
// no mTLS).
func New(certPath, keyPath, caCertPath string) (*CertReloader, error) {
	r := &CertReloader{
		certPath:   certPath,
		keyPath:    keyPath,
		caCertPath: caCertPath,
	}
	if err := r.load(); err != nil {
		return nil, err
	}
	return r, nil
}

// load reads the certificate, key, and optional CA from disk and replaces
// the current values under the write lock.  On failure the old values are
// preserved (so a transient I/O error doesn't cause handshake failures).
func (r *CertReloader) load() error {
	// Check file modification times — skip reload if nothing changed.
	certStat, err := os.Stat(r.certPath)
	if err != nil {
		return fmt.Errorf("stat cert file %s: %w", r.certPath, err)
	}
	keyStat, err := os.Stat(r.keyPath)
	if err != nil {
		return fmt.Errorf("stat key file %s: %w", r.keyPath, err)
	}

	certChanged := !certStat.ModTime().Equal(r.certMod)
	keyChanged := !keyStat.ModTime().Equal(r.keyMod)

	var caStat os.FileInfo
	caChanged := false
	if r.caCertPath != "" {
		var err error
		caStat, err = os.Stat(r.caCertPath)
		if err != nil {
			return fmt.Errorf("stat CA cert file %s: %w", r.caCertPath, err)
		}
		caChanged = !caStat.ModTime().Equal(r.caMod)
	}

	if !certChanged && !keyChanged && !caChanged {
		return nil // nothing changed
	}

	// Load the leaf certificate + private key.
	cert, err := tls.LoadX509KeyPair(r.certPath, r.keyPath)
	if err != nil {
		return fmt.Errorf("load X509 key pair: %w", err)
	}

	// Load optional CA certificate pool for mTLS.
	var caPool *x509.CertPool
	hasCA := false
	if r.caCertPath != "" {
		caPEM, err := os.ReadFile(r.caCertPath)
		if err != nil {
			return fmt.Errorf("read CA cert %s: %w", r.caCertPath, err)
		}
		caPool = x509.NewCertPool()
		if !caPool.AppendCertsFromPEM(caPEM) {
			return fmt.Errorf("parse CA certificate %s: no valid PEM found", r.caCertPath)
		}
		hasCA = true
	}

	// Atomically swap under the write lock.
	r.mu.Lock()
	r.cert = &cert
	r.clientCAPool = caPool
	r.hasCA = hasCA
	r.certMod = certStat.ModTime()
	r.keyMod = keyStat.ModTime()
	if r.caCertPath != "" {
		r.caMod = caStat.ModTime()
	}
	r.mu.Unlock()

	metrics.TLSCertLoadTotal.WithLabelValues("success").Inc()

	log.Info().
		Str("cert_path", r.certPath).
		Bool("mtls", hasCA).
		Msg("TLS certificate reloaded")
	return nil
}

// GetCertificate satisfies tls.Config.GetCertificate.
// It is called by net/http on every TLS handshake and returns the latest
// loaded certificate.
func (r *CertReloader) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cert, nil
}

// GetConfigForClient satisfies tls.Config.GetConfigForClient.
// It is called by gRPC (via credentials.NewTLS) on every new connection and
// returns a fresh tls.Config with the latest certificate and mTLS settings.
func (r *CertReloader) GetConfigForClient(_ *tls.ClientHelloInfo) (*tls.Config, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	cfg := &tls.Config{
		Certificates: []tls.Certificate{*r.cert},
		MinVersion:   tls.VersionTLS13,
	}
	if r.hasCA {
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		cfg.ClientCAs = r.clientCAPool
	}
	return cfg, nil
}

// Reload forces an immediate certificate reload from disk.
// It is safe to call concurrently, e.g. from a SIGHUP handler.
// On failure the error is logged (WARN) and the previous certificate
// continues to be served without interruption.
func (r *CertReloader) Reload() {
	if err := r.load(); err != nil {
		metrics.TLSCertLoadTotal.WithLabelValues("failure").Inc()
		log.Warn().Err(err).Msg("forced certificate reload failed, keeping current cert")
	}
}

// Watch polls the certificate files at the given interval and reloads when
// any file's modification time changes.  It blocks until ctx is cancelled.
// Run it in a background goroutine.
//
// On reload failure, the error is logged (WARN) and the previous certificate
// continues to be served without interruption.
func (r *CertReloader) Watch(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := r.load(); err != nil {
				metrics.TLSCertLoadTotal.WithLabelValues("failure").Inc()
				log.Warn().Err(err).Msg("certificate reload failed, keeping current cert")
			}
		}
	}
}
