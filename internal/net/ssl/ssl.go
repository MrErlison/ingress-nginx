/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package ssl

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1" // #nosec
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/zakjan/cert-chain-resolver/certUtil"
	"k8s.io/apimachinery/pkg/util/sets"

	"k8s.io/ingress-nginx/pkg/apis/ingress"

	ngx_config "k8s.io/ingress-nginx/internal/ingress/controller/config"
	"k8s.io/ingress-nginx/pkg/util/file"

	klog "k8s.io/klog/v2"
)

// FakeSSLCertificateUID defines the default UID to use for the fake SSL
// certificate generated by the ingress controller
var FakeSSLCertificateUID = "00000000-0000-0000-0000-000000000000"

var oidExtensionSubjectAltName = asn1.ObjectIdentifier{2, 5, 29, 17}

const (
	fakeCertificateName = "default-fake-certificate" //#nosec G101
)

// getPemFileName returns absolute file path and file name of pem cert related to given fullSecretName
func getPemFileName(fullSecretName string) (filePath, pemName string) {
	pemName = fmt.Sprintf("%v.pem", fullSecretName)
	return fmt.Sprintf("%v/%v", file.DefaultSSLDirectory, pemName), pemName
}

// CreateSSLCert validates cert and key, extracts common names and returns corresponding SSLCert object
func CreateSSLCert(cert, key []byte, uid string) (*ingress.SSLCert, error) {
	var pemCertBuffer bytes.Buffer
	pemCertBuffer.Write(cert)

	if ngx_config.EnableSSLChainCompletion {
		data, err := fullChainCert(cert)
		if err != nil {
			klog.ErrorS(err, "Error generating certificate chain for Secret")
		} else {
			pemCertBuffer.Reset()
			pemCertBuffer.Write(data)
		}
	}

	pemCertBuffer.WriteString("\n")
	pemCertBuffer.Write(key)

	pemBlock, _ := pem.Decode(pemCertBuffer.Bytes())
	if pemBlock == nil {
		return nil, fmt.Errorf("no valid PEM formatted block found")
	}

	if pemBlock.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("no certificate PEM data found, make sure certificate content starts with 'BEGIN CERTIFICATE'")
	}

	pemCert, err := x509.ParseCertificate(pemBlock.Bytes)
	if err != nil {
		return nil, err
	}

	if _, err := tls.X509KeyPair(cert, key); err != nil {
		return nil, fmt.Errorf("certificate and private key does not have a matching public key: %v", err)
	}

	cn := sets.NewString(pemCert.Subject.CommonName)
	for _, dns := range pemCert.DNSNames {
		if !cn.Has(dns) {
			cn.Insert(dns)
		}
	}

	if len(pemCert.Extensions) > 0 {
		klog.V(3).InfoS("parsing ssl certificate extensions")
		for _, ext := range getExtension(pemCert, oidExtensionSubjectAltName) {
			dns, _, _, err := parseSANExtension(ext.Value)
			if err != nil {
				klog.Warningf("unexpected error parsing certificate extensions: %v", err)
				continue
			}

			for _, dns := range dns {
				if !cn.Has(dns) {
					cn.Insert(dns)
				}
			}
		}
	}

	hasher := sha1.New() // #nosec
	hasher.Write(pemCert.Raw)

	return &ingress.SSLCert{
		Certificate: pemCert,
		PemSHA:      hex.EncodeToString(hasher.Sum(nil)),
		CN:          cn.List(),
		ExpireTime:  pemCert.NotAfter,
		PemCertKey:  pemCertBuffer.String(),
		UID:         uid,
	}, nil
}

// CreateCACert is similar to CreateSSLCert but it creates instance of SSLCert only based on given ca after
// parsing and validating it
func CreateCACert(ca []byte) (*ingress.SSLCert, error) {
	caCert, err := CheckCACert(ca)
	if err != nil {
		return nil, err
	}

	return &ingress.SSLCert{
		CACertificate: caCert,
	}, nil
}

// CheckCACert validates a byte array containing one or more CA certificate/s
func CheckCACert(caBytes []byte) ([]*x509.Certificate, error) {
	certs := []*x509.Certificate{}

	var block *pem.Block
	for {
		block, caBytes = pem.Decode(caBytes)
		if block == nil {
			break
		}

		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, err
		}
		certs = append(certs, cert)
	}

	if len(certs) == 0 {
		return nil, fmt.Errorf("error decoding CA certificate/s")
	}

	return certs, nil
}

// StoreSSLCertOnDisk creates a .pem file with content PemCertKey from the given sslCert
// and sets relevant remaining fields of sslCert object
func StoreSSLCertOnDisk(name string, sslCert *ingress.SSLCert) (string, error) {
	pemFileName, _ := getPemFileName(name)

	err := os.WriteFile(pemFileName, []byte(sslCert.PemCertKey), file.ReadWriteByUser)
	if err != nil {
		return "", fmt.Errorf("could not create PEM certificate file %v: %v", pemFileName, err)
	}

	return pemFileName, nil
}

// ConfigureCACertWithCertAndKey appends ca into existing PEM file consisting of cert and key
// and sets relevant fields in sslCert object
func ConfigureCACertWithCertAndKey(_ string, ca []byte, sslCert *ingress.SSLCert) error {
	var buffer bytes.Buffer

	_, err := buffer.WriteString(sslCert.PemCertKey)
	if err != nil {
		return fmt.Errorf("could not append newline to cert file %v: %v", sslCert.CAFileName, err)
	}

	_, err = buffer.WriteString("\n")
	if err != nil {
		return fmt.Errorf("could not append newline to cert file %v: %v", sslCert.CAFileName, err)
	}

	_, err = buffer.Write(ca)
	if err != nil {
		return fmt.Errorf("could not write ca data to cert file %v: %v", sslCert.CAFileName, err)
	}

	//nolint:gosec // Not change permission to avoid possible issues
	return os.WriteFile(sslCert.CAFileName, buffer.Bytes(), 0o644)
}

// ConfigureCRL creates a CRL file and append it into the SSLCert
func ConfigureCRL(name string, crl []byte, sslCert *ingress.SSLCert) error {
	crlName := fmt.Sprintf("crl-%v.pem", name)
	crlFileName := fmt.Sprintf("%v/%v", file.DefaultSSLDirectory, crlName)

	pemCRLBlock, _ := pem.Decode(crl)
	if pemCRLBlock == nil {
		return fmt.Errorf("no valid PEM formatted block found in CRL %v", name)
	}
	// If the first certificate does not start with 'X509 CRL' it's invalid and must not be used.
	if pemCRLBlock.Type != "X509 CRL" {
		return fmt.Errorf("CRL file %v contains invalid data, and must be created only with PEM formatted certificates", name)
	}

	_, err := x509.ParseRevocationList(pemCRLBlock.Bytes)
	if err != nil {
		return err
	}

	//nolint:gosec // Not change permission to avoid possible issues
	err = os.WriteFile(crlFileName, crl, 0o644)
	if err != nil {
		return fmt.Errorf("could not write CRL file %v: %v", crlFileName, err)
	}

	sslCert.CRLFileName = crlFileName
	sslCert.CRLSHA = file.SHA1(crlFileName)

	return nil
}

// ConfigureCACert is similar to ConfigureCACertWithCertAndKey but it creates a separate file
// for CA cert and writes only ca into it and then sets relevant fields in sslCert
func ConfigureCACert(name string, ca []byte, sslCert *ingress.SSLCert) error {
	caName := fmt.Sprintf("ca-%v.pem", name)
	fileName := fmt.Sprintf("%v/%v", file.DefaultSSLDirectory, caName)

	//nolint:gosec // Not change permission to avoid possible issues
	err := os.WriteFile(fileName, ca, 0o644)
	if err != nil {
		return fmt.Errorf("could not write CA file %v: %v", fileName, err)
	}

	sslCert.CAFileName = fileName

	klog.V(3).InfoS("Created CA Certificate for Authentication", "path", fileName)

	return nil
}

func getExtension(c *x509.Certificate, id asn1.ObjectIdentifier) []pkix.Extension {
	var exts []pkix.Extension
	for _, ext := range c.Extensions {
		if ext.Id.Equal(id) {
			exts = append(exts, ext)
		}
	}
	return exts
}

func parseSANExtension(value []byte) (dnsNames, emailAddresses []string, ipAddresses []net.IP, err error) {
	// RFC 5280, 4.2.1.6

	// SubjectAltName ::= GeneralNames
	//
	// GeneralNames ::= SEQUENCE SIZE (1..MAX) OF GeneralName
	//
	// GeneralName ::= CHOICE {
	//      otherName                       [0]     OtherName,
	//      rfc822Name                      [1]     IA5String,
	//      dNSName                         [2]     IA5String,
	//      x400Address                     [3]     ORAddress,
	//      directoryName                   [4]     Name,
	//      ediPartyName                    [5]     EDIPartyName,
	//      uniformResourceIdentifier       [6]     IA5String,
	//      iPAddress                       [7]     OCTET STRING,
	//      registeredID                    [8]     OBJECT IDENTIFIER }
	var seq asn1.RawValue
	var rest []byte
	if rest, err = asn1.Unmarshal(value, &seq); err != nil {
		return dnsNames, emailAddresses, ipAddresses, err
	} else if len(rest) != 0 {
		err = errors.New("x509: trailing data after X.509 extension")
		return dnsNames, emailAddresses, ipAddresses, err
	}
	if !seq.IsCompound || seq.Tag != 16 || seq.Class != 0 {
		err = asn1.StructuralError{Msg: "bad SAN sequence"}
		return dnsNames, emailAddresses, ipAddresses, err
	}

	rest = seq.Bytes
	for len(rest) > 0 {
		var v asn1.RawValue
		rest, err = asn1.Unmarshal(rest, &v)
		if err != nil {
			return dnsNames, emailAddresses, ipAddresses, err
		}
		switch v.Tag {
		case 1:
			emailAddresses = append(emailAddresses, string(v.Bytes))
		case 2:
			dnsNames = append(dnsNames, string(v.Bytes))
		case 7:
			switch len(v.Bytes) {
			case net.IPv4len, net.IPv6len:
				ipAddresses = append(ipAddresses, v.Bytes)
			default:
				err = errors.New("x509: certificate contained IP address of length " + strconv.Itoa(len(v.Bytes)))
				return dnsNames, emailAddresses, ipAddresses, err
			}
		}
	}

	return dnsNames, emailAddresses, ipAddresses, err
}

// AddOrUpdateDHParam creates a dh parameters file with the specified name
func AddOrUpdateDHParam(name string, dh []byte) (string, error) {
	pemFileName, pemName := getPemFileName(name)

	tempPemFile, err := os.CreateTemp(file.DefaultSSLDirectory, pemName)

	klog.V(3).InfoS("Creating temporal file for DH", "path", tempPemFile.Name(), "name", pemName)
	if err != nil {
		return "", fmt.Errorf("could not create temp pem file %v: %v", pemFileName, err)
	}

	_, err = tempPemFile.Write(dh)
	if err != nil {
		return "", fmt.Errorf("could not write to pem file %v: %v", tempPemFile.Name(), err)
	}

	err = tempPemFile.Close()
	if err != nil {
		return "", fmt.Errorf("could not close temp pem file %v: %v", tempPemFile.Name(), err)
	}

	defer os.Remove(tempPemFile.Name())

	pemCerts, err := os.ReadFile(tempPemFile.Name())
	if err != nil {
		return "", err
	}

	pemBlock, _ := pem.Decode(pemCerts)
	if pemBlock == nil {
		return "", fmt.Errorf("no valid PEM formatted block found")
	}

	// If the file does not start with 'BEGIN DH PARAMETERS' it's invalid and must not be used.
	if pemBlock.Type != "DH PARAMETERS" {
		return "", fmt.Errorf("certificate %v contains invalid data", name)
	}

	err = os.Rename(tempPemFile.Name(), pemFileName)
	if err != nil {
		return "", fmt.Errorf("could not move temp pem file %v to destination %v: %v", tempPemFile.Name(), pemFileName, err)
	}

	return pemFileName, nil
}

// GetFakeSSLCert creates a Self Signed Certificate
// Based in the code https://golang.org/src/crypto/tls/generate_cert.go
func GetFakeSSLCert() *ingress.SSLCert {
	cert, key := getFakeHostSSLCert("ingress.local")

	sslCert, err := CreateSSLCert(cert, key, FakeSSLCertificateUID)
	if err != nil {
		klog.Fatalf("unexpected error creating fake SSL Cert: %v", err)
	}

	path, err := StoreSSLCertOnDisk(fakeCertificateName, sslCert)
	if err != nil {
		klog.Fatalf("unexpected error storing fake SSL Cert: %v", err)
	}

	sslCert.PemFileName = path
	sslCert.PemSHA = file.SHA1(path)

	return sslCert
}

func getFakeHostSSLCert(host string) (cert, key []byte) {
	var priv interface{}
	var err error

	priv, err = rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		klog.Fatalf("failed to generate fake private key: %v", err)
	}

	notBefore := time.Now()
	// This certificate is valid for 365 days
	notAfter := notBefore.Add(365 * 24 * time.Hour)

	serialNumberLimit := new(big.Int).Lsh(big.NewInt(1), 128)
	serialNumber, err := rand.Int(rand.Reader, serialNumberLimit)
	if err != nil {
		klog.Fatalf("failed to generate fake serial number: %v", err)
	}

	template := x509.Certificate{
		SerialNumber: serialNumber,
		Subject: pkix.Name{
			Organization: []string{"Acme Co"},
			CommonName:   "Kubernetes Ingress Controller Fake Certificate",
		},
		NotBefore: notBefore,
		NotAfter:  notAfter,

		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		DNSNames:              []string{host},
	}
	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &priv.(*rsa.PrivateKey).PublicKey, priv)
	if err != nil {
		klog.Fatalf("Failed to create fake certificate: %v", err)
	}

	cert = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})

	key = pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(priv.(*rsa.PrivateKey))})

	return cert, key
}

// fullChainCert checks if a certificate file contains issues in the intermediate CA chain
// Returns a new certificate with the intermediate certificates.
// If the certificate does not contain issues with the chain it returns an empty byte array
func fullChainCert(in []byte) ([]byte, error) {
	cert, err := certUtil.DecodeCertificate(in)
	if err != nil {
		return nil, err
	}

	certPool := x509.NewCertPool()
	certPool.AddCert(cert)

	_, err = cert.Verify(x509.VerifyOptions{
		Intermediates: certPool,
	})
	if err == nil {
		return nil, nil
	}

	certs, err := certUtil.FetchCertificateChain(cert)
	if err != nil {
		return nil, err
	}

	return certUtil.EncodeCertificates(certs), nil
}

// IsValidHostname checks if a hostname is valid in a list of common names
func IsValidHostname(hostname string, commonNames []string) bool {
	for _, cn := range commonNames {
		if strings.EqualFold(hostname, cn) {
			return true
		}

		labels := strings.Split(hostname, ".")
		labels[0] = "*"
		candidate := strings.Join(labels, ".")
		if strings.EqualFold(candidate, cn) {
			return true
		}
	}

	return false
}

// TLSListener implements a dynamic certificate loader
type TLSListener struct {
	certificatePath string
	keyPath         string
	certificate     *tls.Certificate
	err             error
	lock            sync.Mutex
}

// NewTLSListener watches changes to th certificate and key paths
// and reloads it whenever it changes
func NewTLSListener(certificate, key string) *TLSListener {
	l := TLSListener{
		certificatePath: certificate,
		keyPath:         key,
		lock:            sync.Mutex{},
	}

	l.load()

	_, err := file.NewFileWatcher(certificate, l.load)
	if err != nil {
		klog.Errorf("unexpected error: %v", err)
	}
	_, err = file.NewFileWatcher(key, l.load)
	if err != nil {
		klog.Errorf("unexpected error: %v", err)
	}
	return &l
}

// GetCertificate implements the tls.Config.GetCertificate interface
func (tl *TLSListener) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	tl.lock.Lock()
	defer tl.lock.Unlock()
	return tl.certificate, tl.err
}

// TLSConfig instantiates a TLS configuration, always providing an up to date certificate
func (tl *TLSListener) TLSConfig() *tls.Config {
	return &tls.Config{
		GetCertificate: tl.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}
}

func (tl *TLSListener) load() {
	klog.InfoS("loading tls certificate", "path", tl.certificatePath, "key", tl.keyPath)
	certBytes, err := os.ReadFile(tl.certificatePath)
	if err != nil {
		tl.certificate = nil
		tl.err = err
	}
	keyBytes, err := os.ReadFile(tl.keyPath)
	if err != nil {
		tl.certificate = nil
		tl.err = err
	}
	cert, err := tls.X509KeyPair(certBytes, keyBytes)
	tl.lock.Lock()
	defer tl.lock.Unlock()
	tl.certificate, tl.err = &cert, err
}
