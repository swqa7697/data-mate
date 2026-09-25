// Package transport constructs explicit connection routes without ambient defaults.
package transport

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"github.com/swqa7697/data-mate/internal/config"
	"io"
	"os"
)

// TLSConfig verifies the original database hostname. There is no plaintext fallback.
func TLSConfig(host string, settings config.TLS) (*tls.Config, error) {
	if settings.Mode == "disabled" && settings.CAFile == "" {
		return nil, nil
	}
	if settings.Mode != "verify-full" {
		return nil, errors.New("invalid TLS configuration")
	}
	var roots *x509.CertPool
	if settings.CAFile != "" {
		f, err := os.Open(settings.CAFile)
		if err != nil {
			return nil, errors.New("cannot read TLS CA")
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
			return nil, errors.New("invalid TLS CA file")
		}
		b, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
		if err != nil || len(b) > 1<<20 {
			return nil, errors.New("cannot read TLS CA")
		}
		roots = x509.NewCertPool()
		if !roots.AppendCertsFromPEM(b) {
			return nil, errors.New("invalid TLS CA")
		}
	}
	return &tls.Config{MinVersion: tls.VersionTLS12, ServerName: host, RootCAs: roots}, nil
}
