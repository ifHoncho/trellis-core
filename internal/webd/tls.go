package webd

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"meridian/trellis/internal/fsutil"
)

// The UI is served over HTTPS with a certificate the node signs itself
// (architecture §11): there is no authority to ask on a mesh with no
// internet. Browsers warn once and remember the exception; the
// certificate is therefore kept for ten years and never replaced on its
// own, so that the remembered one stays valid. To use a certificate of
// your own, put it in the state directory under the same file names.

const (
	certFile = "tls-cert.pem"
	keyFile  = "tls-key.pem"
)

// certNames are the names and addresses the certificate is made for.
func (s *Server) certNames(ctx context.Context) (dns []string, ips []net.IP) {
	dns = []string{"node.mesh", "localhost"}
	ips = []net.IP{net.IPv4(127, 0, 0, 1)}
	info, ok := s.webInfo(ctx)
	prefix, err := netip.ParsePrefix(info.Prefix)
	if !ok || err != nil {
		prefix = defaultPrefix
	}
	// The anycast gateway address: the first address of the mesh prefix.
	ips = append(ips, net.IP(prefix.Masked().Addr().Next().AsSlice()))
	if ok && info.Callsign != "" {
		dns = append(dns, info.Callsign+".mesh")
	}
	return dns, ips
}

func loadOrCreateCert(dir string, names func() ([]string, []net.IP), log *slog.Logger) (*tls.Certificate, error) {
	if dir != "" {
		fsutil.RemoveStale(dir) // one webd per state directory
		cert, err := tls.LoadX509KeyPair(filepath.Join(dir, certFile), filepath.Join(dir, keyFile))
		if err == nil {
			return &cert, nil
		}
		if !os.IsNotExist(err) {
			log.Warn("cannot use the stored TLS certificate; making a new one", "dir", dir, "err", err)
		}
	}
	dns, ips := names()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "Trellis node", Organization: []string{"Meridian Trellis"}},
		NotBefore:    time.Now().Add(-24 * time.Hour), // a node's clock may be behind
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     dns, IPAddresses: ips,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err == nil {
			err = fsutil.WriteFile(filepath.Join(dir, keyFile), keyPEM, 0o600)
			if err == nil {
				err = fsutil.WriteFile(filepath.Join(dir, certFile), certPEM, 0o644)
			}
			if err != nil {
				log.Warn("cannot store the TLS certificate; browsers will warn again after a restart", "dir", dir, "err", err)
			} else {
				log.Info("made a self-signed TLS certificate", "dir", dir, "names", dns)
			}
		}
	} else {
		log.Info("made a self-signed TLS certificate for this run only (no state directory)")
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	return &cert, nil
}
