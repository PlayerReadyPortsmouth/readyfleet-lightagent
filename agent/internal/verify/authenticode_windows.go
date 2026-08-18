//go:build windows

package verify

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// VerifyAuthenticode returns nil only if the file at path carries a Valid
// Authenticode signature and the SHA-256 fingerprint of the actual signer
// certificate DER matches expectedFingerprint. Any other status, missing
// signer certificate, or signer mismatch is a hard failure.
// The MSI install, the agent self-update, and the SolarBeam engine update
// all gate on this before running/serving a downloaded binary on a venue or
// personal PC — it is defence in depth over the sha256 check.
//
// This calls WinVerifyTrust and the crypt32 message APIs directly via
// golang.org/x/sys/windows — no subprocess, no PowerShell. The previous
// implementation shelled out to `powershell.exe -Command
// Get-AuthenticodeSignature`, which — root-caused live on a real Windows
// box this session — reliably fails to autoload the built-in
// Microsoft.PowerShell.Security module specifically when the parent
// process is a compiled Go binary rather than an interactive shell (a
// minimal, isolated repro confirmed this independent of the module
// analysis cache, stale processes, or stdin inheritance, all ruled out).
// Removing the PowerShell dependency from this security-critical path
// makes that entire class of failure structurally impossible rather than
// papering over one manifestation of it.
//
// Authenticode verification is only fully provable on real Windows signing
// infrastructure (a cert in the machine trust store, chaining to a trusted
// root); it cannot be exercised off-Windows.
func VerifyAuthenticode(path, expectedFingerprint string) error {
	return verifyAuthenticode(path, expectedFingerprint, false)
}

// certEUntrustedRoot is CERT_E_UNTRUSTEDROOT (winerror.h) — the exact
// HRESULT WinVerifyTrust returns when a signature's certificate chain is
// otherwise cryptographically valid but doesn't terminate in a root the
// local machine trusts. Not exported by golang.org/x/sys/windows; the
// value is fixed by the Windows Authenticode ABI, not by this codebase.
const certEUntrustedRoot = windows.Errno(0x800B0109)

// VerifyAuthenticodeAllowUntrustedRoot behaves exactly like
// VerifyAuthenticode — same signer-fingerprint pin, same hard failure on
// any other status — except it additionally accepts the one specific case
// where the signature and chain are cryptographically valid but the
// private ReadyFleet root isn't in the machine's trust store yet. This is
// for the managed bootstrap installer's fresh-venue-machine case only: a
// brand new PC has no reason to already trust an internal CA it's never
// seen, and the managed installer is the one place that first-contact
// bootstrapping has to happen locally rather than being a precondition
// (unlike BYOD, where the mentor explicitly imports the CA before this
// ever runs — see docs/runbooks/byod-light-agent.md). Exact content and
// signer fingerprint pinning apply identically either way; only the
// trust-chain leniency differs.
func VerifyAuthenticodeAllowUntrustedRoot(path, expectedFingerprint string) error {
	return verifyAuthenticode(path, expectedFingerprint, true)
}

func verifyAuthenticode(path, expectedFingerprint string, allowUntrustedRoot bool) error {
	path16, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return fmt.Errorf("authenticode: encode path: %w", err)
	}

	trustErr := verifyTrust(path16)
	if trustErr != nil && !(allowUntrustedRoot && errors.Is(trustErr, certEUntrustedRoot)) {
		return fmt.Errorf("authenticode: signature status invalid: %w", trustErr)
	}

	certDER, err := signerCertificateDER(path16)
	if err != nil {
		return fmt.Errorf("authenticode: extract signer certificate: %w", err)
	}
	return VerifySignerCertificateDER(certDER, expectedFingerprint)
}

// verifyTrust asks the OS's own trust provider chain (the exact mechanism
// behind "This file came from an untrusted publisher" prompts) whether
// path's embedded Authenticode signature is valid and chains to a trusted
// root. WTD_UI_NONE means this never shows UI itself; WTD_REVOKE_NONE skips
// revocation/CRL network checks — the same posture the prior PowerShell
// implementation had (Get-AuthenticodeSignature does not check revocation
// by default either), appropriate here since the pinned-fingerprint check
// right after this is the actual authority, not the chain-trust check
// alone.
func verifyTrust(path16 *uint16) error {
	fileInfo := &windows.WinTrustFileInfo{
		Size:     uint32(unsafe.Sizeof(windows.WinTrustFileInfo{})),
		FilePath: path16,
	}
	data := &windows.WinTrustData{
		Size:                            uint32(unsafe.Sizeof(windows.WinTrustData{})),
		UIChoice:                        windows.WTD_UI_NONE,
		RevocationChecks:                windows.WTD_REVOKE_NONE,
		UnionChoice:                     windows.WTD_CHOICE_FILE,
		StateAction:                     windows.WTD_STATEACTION_VERIFY,
		FileOrCatalogOrBlobOrSgnrOrCert: unsafe.Pointer(fileInfo),
	}
	verifyErr := windows.WinVerifyTrustEx(windows.InvalidHWND, &windows.WINTRUST_ACTION_GENERIC_VERIFY_V2, data)
	// The state handle WinVerifyTrust allocated on the VERIFY call above
	// must always be released with a matching CLOSE call, independent of
	// whether verification succeeded — this is not a defer-cleanup nicety,
	// it is what the WinTrust API contract requires to avoid leaking the
	// provider state.
	data.StateAction = windows.WTD_STATEACTION_CLOSE
	_ = windows.WinVerifyTrustEx(windows.InvalidHWND, &windows.WINTRUST_ACTION_GENERIC_VERIFY_V2, data)
	return verifyErr
}

// signerCertificateDER extracts the raw DER bytes of the leaf certificate
// that actually signed path, for our own fingerprint pin — independent of
// whatever trust decision verifyTrust made. Opens the file's embedded
// PKCS#7 SignedData blob (CryptQueryObject), reads the signer's identity
// out of it (CryptMsgGetParam, CMSG_SIGNER_CERT_INFO_PARAM — the standard,
// ABI-stable pattern for this; deliberately not
// WTHelperProvDataFromStateData, whose CRYPT_PROVIDER_DATA struct is large,
// opaque, and version-sensitive), then looks that identity up in the
// message's associated certificate store to get the actual certificate
// bytes.
func signerCertificateDER(path16 *uint16) ([]byte, error) {
	var certStore, msg windows.Handle
	var msgAndCertEncodingType, contentType, formatType uint32
	if err := windows.CryptQueryObject(
		windows.CERT_QUERY_OBJECT_FILE,
		unsafe.Pointer(path16),
		windows.CERT_QUERY_CONTENT_FLAG_PKCS7_SIGNED_EMBED,
		windows.CERT_QUERY_FORMAT_FLAG_BINARY,
		0,
		&msgAndCertEncodingType,
		&contentType,
		&formatType,
		&certStore,
		&msg,
		nil,
	); err != nil {
		return nil, fmt.Errorf("cryptqueryobject: %w", err)
	}
	defer windows.CertCloseStore(certStore, 0)
	defer cryptMsgClose(msg)

	signerInfo, err := cryptMsgSignerCertInfo(msg)
	if err != nil {
		return nil, err
	}

	cert, err := windows.CertFindCertificateInStore(
		certStore,
		windows.X509_ASN_ENCODING|windows.PKCS_7_ASN_ENCODING,
		0,
		windows.CERT_FIND_SUBJECT_CERT,
		unsafe.Pointer(&signerInfo[0]),
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("certfindcertificateinstore: %w", err)
	}
	defer windows.CertFreeCertificateContext(cert)

	der := make([]byte, cert.Length)
	copy(der, unsafe.Slice(cert.EncodedCert, cert.Length))
	return der, nil
}

// cmsgSignerCertInfoParam asks CryptMsgGetParam for the CERT_INFO
// (Issuer + SerialNumber) of the message's first (index 0) signer — this
// library only ever verifies singly-signed release artifacts.
const cmsgSignerCertInfoParam = 7

var (
	modcrypt32           = windows.NewLazySystemDLL("crypt32.dll")
	procCryptMsgGetParam = modcrypt32.NewProc("CryptMsgGetParam")
	procCryptMsgClose    = modcrypt32.NewProc("CryptMsgClose")
)

// cryptMsgSignerCertInfo returns the raw bytes of the signer's CERT_INFO
// structure (issuer + serial number) — the standard two-call pattern
// (size probe, then a real call into a buffer sized to match) since
// CryptMsgGetParam does not expose its output size ahead of time.
func cryptMsgSignerCertInfo(msg windows.Handle) ([]byte, error) {
	var size uint32
	if ret, _, err := procCryptMsgGetParam.Call(
		uintptr(msg), uintptr(cmsgSignerCertInfoParam), 0,
		0, uintptr(unsafe.Pointer(&size)),
	); ret == 0 {
		return nil, fmt.Errorf("cryptmsggetparam(size): %w", err)
	}
	buf := make([]byte, size)
	if ret, _, err := procCryptMsgGetParam.Call(
		uintptr(msg), uintptr(cmsgSignerCertInfoParam), 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&size)),
	); ret == 0 {
		return nil, fmt.Errorf("cryptmsggetparam: %w", err)
	}
	return buf, nil
}

func cryptMsgClose(msg windows.Handle) {
	_, _, _ = procCryptMsgClose.Call(uintptr(msg))
}
