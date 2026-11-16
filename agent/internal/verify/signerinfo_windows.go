//go:build windows

package verify

import (
	"crypto/x509"

	"golang.org/x/sys/windows"
)

// SignerInfo reads (does not pin) the Authenticode signer of path for
// display: the leaf certificate's subject common name (or organisation) and
// whether Windows trusts the signature. An unsigned or unreadable file
// returns ("", false). Used by the elevation watcher to describe programs;
// never use it to authorise anything.
func SignerInfo(path string) (subject string, valid bool) {
	p16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", false
	}
	der, err := signerCertificateDER(p16)
	if err != nil {
		return "", false
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return "", false
	}
	subject = cert.Subject.CommonName
	if subject == "" && len(cert.Subject.Organization) > 0 {
		subject = cert.Subject.Organization[0]
	}
	return subject, verifyTrust(p16) == nil
}
