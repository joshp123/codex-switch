package relay

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// ClaudeProxyCAPath is trusted only by clients configured with NODE_EXTRA_CA_CERTS.
func ClaudeProxyCAPath(dataDirectory string) string {
	return filepath.Join(dataDirectory, "claude-proxy-ca.pem")
}

// CreateClaudeProxyCertificate retains one authority across process generations.
// Its certificate and private key share a private file so they cannot diverge.
func CreateClaudeProxyCertificate(dataDirectory string) (tls.Certificate, error) {
	if err := os.MkdirAll(dataDirectory, 0o700); err != nil {
		return tls.Certificate{}, err
	}
	lock, err := os.OpenFile(filepath.Join(dataDirectory, "claude-proxy-ca.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return tls.Certificate{}, err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return tls.Certificate{}, err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)

	privatePath := filepath.Join(dataDirectory, "claude-proxy-authority.pem")
	authorityPEM, err := os.ReadFile(privatePath)
	if os.IsNotExist(err) {
		authorityPEM, err = generateClaudeProxyAuthority()
		if err == nil {
			err = writeClaudeAuthorityFile(privatePath, authorityPEM)
		}
	}
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("create Claude proxy authority: %w", err)
	}
	if err := os.Chmod(privatePath, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	authority, err := tls.X509KeyPair(authorityPEM, authorityPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read Claude proxy authority: %w", err)
	}
	root, err := x509.ParseCertificate(authority.Certificate[0])
	if err != nil {
		return tls.Certificate{}, err
	}
	if !time.Now().Before(root.NotAfter) {
		authorityPEM, err = generateClaudeProxyAuthority()
		if err == nil {
			err = writeClaudeAuthorityFile(privatePath, authorityPEM)
		}
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("renew Claude proxy authority: %w", err)
		}
		if err := os.Chmod(privatePath, 0o600); err != nil {
			return tls.Certificate{}, err
		}
		authority, err = tls.X509KeyPair(authorityPEM, authorityPEM)
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("read renewed Claude proxy authority: %w", err)
		}
		root, err = x509.ParseCertificate(authority.Certificate[0])
		if err != nil {
			return tls.Certificate{}, err
		}
	}
	publicPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root.Raw})
	publicPath := ClaudeProxyCAPath(dataDirectory)
	existing, err := os.ReadFile(publicPath)
	if err != nil && !os.IsNotExist(err) {
		return tls.Certificate{}, err
	}
	if !bytes.Equal(existing, publicPEM) {
		if err := writeClaudeAuthorityFile(publicPath, publicPEM); err != nil {
			return tls.Certificate{}, err
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}
	now := time.Now()
	certificate, err := x509.CreateCertificate(rand.Reader, &x509.Certificate{
		SerialNumber: serial, DNSNames: []string{"api.anthropic.com"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(1, 0, 0),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}, root, &key.PublicKey, authority.PrivateKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{certificate, root.Raw}, PrivateKey: key}, nil
}

func generateClaudeProxyAuthority() ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "Codex Switch Claude proxy"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.AddDate(10, 0, 0),
		IsCA: true, BasicConstraintsValid: true,
		KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	certificate, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	privateKey, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return append(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateKey})...), nil
}

func writeClaudeAuthorityFile(path string, contents []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".claude-proxy-ca-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, err = file.Write(contents)
	closeError := file.Close()
	if err != nil {
		return err
	}
	if closeError != nil {
		return closeError
	}
	return os.Rename(file.Name(), path)
}
