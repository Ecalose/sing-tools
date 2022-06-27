package selfsign

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	mRand "math/rand"
	"net"
	"time"

	"github.com/sagernet/sing/common/random"
)

func GenerateCertificate(hosts ...string) (*tls.Certificate, error) {
	r := mRand.New(random.Source{Reader: rand.Reader})

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}

	createAt := time.Now().AddDate(0, -r.Intn(2)-1, -mRand.Intn(15))
	createAt = createAt.Add(-(time.Duration(r.Intn(12)) * time.Hour))
	createAt = createAt.Add(-(time.Duration(r.Intn(60)) * time.Minute))
	createAt = createAt.Add(-(time.Duration(r.Intn(60)) * time.Second))
	createAt = createAt.Add(-(time.Duration(r.Intn(1000)) * time.Millisecond))
	createAt = createAt.Add(-(time.Duration(r.Intn(1000)) * time.Microsecond))

	endAt := createAt.AddDate(0, (r.Intn(1)+1)*6, 0)

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		return nil, err
	}
	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Cloudflare, Inc."},
		},
		NotBefore: createAt,
		NotAfter:  endAt,

		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	for _, h := range hosts {
		if ip := net.ParseIP(h); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else {
			template.DNSNames = append(template.DNSNames, h)
		}
	}

	template.Raw, err = x509.CreateCertificate(rand.Reader, &template, &template, &privateKey.PublicKey, privateKey)
	if err != nil {
		return nil, err
	}
	privBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		return nil, err
	}

	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: template.Raw}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privBytes}),
	)
	if err != nil {
		return nil, err
	}

	return &cert, nil
}
