// Package tlsconf monta configurações TLS endurecidas (TLS 1.3, mTLS obrigatório nos servidores).
package tlsconf

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
)

func loadPool(caFile string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caFile) // #nosec G304 G703 -- caminho vem da configuração do operador, não de entrada externa
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("no certificates found in %s", caFile)
	}
	return pool, nil
}

// reloader relê o par certificado/chave a cada handshake de nova conexão se o arquivo mudou
// (rotação de certificados sem reinício).
type reloader struct {
	certFile, keyFile string
}

func (r *reloader) get(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c, err := tls.LoadX509KeyPair(r.certFile, r.keyFile)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (r *reloader) getClient(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	return r.get(nil)
}

// Server exige certificado de cliente válido assinado pela CA informada.
func Server(certFile, keyFile, clientCAFile string) (*tls.Config, error) {
	if certFile == "" || keyFile == "" || clientCAFile == "" {
		return nil, errors.New("tls cert, key and client CA are all required")
	}
	r := &reloader{certFile, keyFile}
	if _, err := r.get(nil); err != nil {
		return nil, fmt.Errorf("load server cert: %w", err)
	}
	pool, err := loadPool(clientCAFile)
	if err != nil {
		return nil, fmt.Errorf("load client CA: %w", err)
	}
	return &tls.Config{
		MinVersion:             tls.VersionTLS13,
		GetCertificate:         r.get,
		ClientAuth:             tls.RequireAndVerifyClientCert,
		ClientCAs:              pool,
		SessionTicketsDisabled: true,
	}, nil
}

// Client usa mTLS quando cert/key são informados e valida o servidor pela CA.
func Client(certFile, keyFile, serverCAFile, serverName string) (*tls.Config, error) {
	pool, err := loadPool(serverCAFile)
	if err != nil {
		return nil, fmt.Errorf("load server CA: %w", err)
	}
	cfg := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: pool, ServerName: serverName}
	if certFile != "" || keyFile != "" {
		r := &reloader{certFile, keyFile}
		if _, err := r.get(nil); err != nil {
			return nil, fmt.Errorf("load client cert: %w", err)
		}
		cfg.GetClientCertificate = r.getClient
	}
	return cfg, nil
}
