// Package identity manages a device's long-lived key pair, its self-signed
// TLS certificate, and the device ID derived from the public key.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base32"
	"encoding/binary"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

// IDLen is the length of a device ID string.
const IDLen = 52

var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Identity is this device's key material and derived ID.
type Identity struct {
	Cert tls.Certificate
	ID   string
}

// LoadOrCreate loads the identity stored in dir, creating a new Ed25519 key
// and certificate on first use. name only labels a newly created certificate.
func LoadOrCreate(dir, name string) (*Identity, error) {
	keyPath := filepath.Join(dir, "key.pem")
	certPath := filepath.Join(dir, "cert.pem")

	cert, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err == nil {
		return fromCert(cert)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("load identity: %w", err)
	}

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	certPEM, keyPEM, err := generate(name)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		return nil, err
	}
	cert, err = tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return fromCert(cert)
}

func fromCert(cert tls.Certificate) (*Identity, error) {
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	cert.Leaf = leaf
	return &Identity{Cert: cert, ID: IDFromCert(leaf)}, nil
}

func generate(name string) (certPEM, keyPEM []byte, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(100, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, priv)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// IDFromCert derives a device ID from the certificate's public key, so the ID
// survives certificate regeneration with the same key.
func IDFromCert(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.RawSubjectPublicKeyInfo)
	return idEncoding.EncodeToString(sum[:])
}

// ValidID reports whether s has the shape of a device ID.
func ValidID(s string) bool {
	if len(s) != IDLen {
		return false
	}
	_, err := idEncoding.DecodeString(s)
	return err == nil
}

// Short returns the abbreviated form of an ID used in human-facing output.
func Short(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}

// PairingCode returns the six-digit code both sides of a pairing display. It
// depends only on the two device IDs, so a man-in-the-middle presenting its
// own certificate to either side produces mismatching codes.
func PairingCode(a, b string) string {
	if a > b {
		a, b = b, a
	}
	sum := sha256.Sum256([]byte("messh-pair-v1\x00" + a + "\x00" + b))
	n := binary.BigEndian.Uint32(sum[:4]) % 1_000_000
	return fmt.Sprintf("%03d-%03d", n/1000, n%1000)
}
