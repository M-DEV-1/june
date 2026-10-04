//go:build windows

package update

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	crypt32              = windows.NewLazySystemDLL("crypt32.dll")
	procCryptMsgGetParam = crypt32.NewProc("CryptMsgGetParam")
	procCryptMsgClose    = crypt32.NewProc("CryptMsgClose")
)

const (
	// cmsgSignerCertInfoParam asks CryptMsgGetParam for the signer's issuer and serial number, as a CERT_INFO, which is what finds the signer's certificate among those the signature carries.
	cmsgSignerCertInfoParam = 7
	oidCommonName           = "2.5.4.3"
	oidOrganization         = "2.5.4.10"
	// certX500NameStr asks CertGetNameString for a whole distinguished name, every attribute in order ("CN=…, O=…, L=…, C=…"), as CertNameToStr's CERT_X500_NAME_STR writes it.
	certX500NameStr = 3
	// installerProduct is the ProductName june.iss writes into the installer's version resource (VersionInfoProductName). The resource is inside what the signature covers, and SignPath signs a file for June's project only when its product name is the one that project's artifact configuration names (packaging/signpath/installer.xml), so a signed installer calling itself June went through June's signing, or through another project SignPath let claim the same name.
	installerProduct = "June"
)

// publisher is who signed a file: the whole subject of the signing certificate, the CA that issued it and the root its chain ends in, which are what is compared, and the certificate's common name and organization, which are what a person is shown.
type publisher struct {
	subject, issuer, root string
	cn, org               string
}

func (p publisher) String() string {
	switch {
	case p.cn == "":
		return p.subject
	case p.org == "" || p.org == p.cn:
		return p.cn
	}
	return p.cn + " (" + p.org + ")"
}

// ownPublisher reads who signed the June that is running, which samePublisher then holds the downloaded installer to. It runs before anything is downloaded: the reasons it fails (root updates switched off, a certificate expired with no timestamp, a policy that stops the check) last, and found only after the download they would fetch the whole installer on every press of Update or Try again, only to delete it. Output: the publisher; nil with no error when this June carries no signature at all; or why June will not install an update itself.
func ownPublisher() (*publisher, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, &userError{"Couldn't check who published the June you have, so nothing was downloaded. Download the update from the release page instead.", err}
	}
	want, err := signedBy(self)
	switch {
	case errors.Is(err, syscall.Errno(windows.TRUST_E_NOSIGNATURE)):
		slog.Info("update: this June is not signed, so the installer is checked against the release's checksums only", "exe", self)
		return nil, nil
	case err != nil:
		return nil, &userError{"Couldn't confirm who signed the June you have, so June won't download and run an update itself. Download it from the release page instead.", err}
	}
	return &want, nil
}

// samePublisher holds a downloaded installer to the publisher of the June that is running: when this June is signed, the installer must carry a signature Windows trusts, from a certificate with the same whole subject, issued by the same CA or chaining to the same root, on a file whose product name is June's. Input: the verified download, and ownPublisher's answer (nil for a June with no signature). Output: nil, or why it must not be run.
//
// What each part buys, and what it does not:
//   - SHA256SUMS and GitHub's digest come from the same release as the installer, so they catch a damaged download but not an installer that someone able to publish a release uploaded. A signature needs the signing service, which an upload does not reach.
//   - The whole subject is compared rather than the common name and organization alone, so a certificate some CA issued to another organization of the same name, at another address, does not pass; a renewal keeps the subject. The CA is compared too, so the same subject from a different CA does not pass either: either the intermediate that issued the certificate or the root its chain ends in must be this June's. Either is enough because each moves on its own: a CA rotates its intermediates every few years under the same root, and a root reached through a cross-signed certificate depends on which certificates the signature carries. A certificate whose subject changed, or that moved to another CA, makes this June refuse the update once; that user downloads the next June by hand, and it updates itself from then on.
//   - SignPath Foundation signs every project in its open-source programme with one certificate in its own name, so a file another of those projects had signed has the same subject and root. What ties a signed file to June is its product name (see installerProduct).
//   - A June without a signature (every release until signing is set up, and every local build) has no publisher to hold the installer to and is left with the checksums: on such a release, whoever can publish to the repository's releases can get their installer run by everyone who presses Update. That is the same exposure as the manual download from the same release page, no better.
//   - Only a June that carries no signature at all gets that fallback. A signed June whose own signature Windows cannot verify right now (a certificate expired with no timestamp, a root missing because root updates are switched off, a damaged file) refuses the update instead (ownPublisher), because otherwise anything that breaks the check of this June would switch the check of the installer off with it.
func samePublisher(path string, want *publisher) error {
	if want == nil {
		return nil
	}
	got, err := signedBy(path)
	if err != nil {
		return &userError{"The downloaded installer isn't signed by " + want.String() + ", who signed the June you have, so it was deleted and not run.", err}
	}
	if got.subject != want.subject {
		return &userError{shown: fmt.Sprintf("The downloaded installer is signed by %s, not by %s, who signed the June you have, so it was deleted and not run.", got, want)}
	}
	if got.issuer != want.issuer && got.root != want.root {
		slog.Warn("update: the installer's certificate comes from a different CA from this June's", "installer_issuer", got.issuer, "installer_root", got.root, "june_issuer", want.issuer, "june_root", want.root)
		return &userError{shown: fmt.Sprintf("The downloaded installer is signed by %s, but through a different certificate authority from the June you have, so it was deleted and not run. Download it from the release page instead.", got)}
	}
	name, err := productName(path)
	if err != nil {
		return &userError{"The downloaded installer doesn't say what it installs, so it was deleted and not run.", err}
	}
	if name != installerProduct {
		return &userError{shown: fmt.Sprintf("The downloaded installer is signed by %s but installs %q, not June, so it was deleted and not run.", got, name)}
	}
	slog.Info("update: the installer is June's, signed by the publisher of this June", "publisher", got.subject, "issuer", got.issuer, "root", got.root)
	return nil
}

// productName reads the ProductName in a file's version resource, under the first language the resource lists. Output: the name without surrounding blanks, or the error.
func productName(path string) (string, error) {
	size, err := windows.GetFileVersionInfoSize(path, nil)
	if err != nil {
		return "", err
	}
	info := make([]byte, size)
	if err := windows.GetFileVersionInfo(path, 0, size, unsafe.Pointer(&info[0])); err != nil {
		return "", err
	}
	var langs unsafe.Pointer
	var n uint32
	if err := windows.VerQueryValue(unsafe.Pointer(&info[0]), `\VarFileInfo\Translation`, unsafe.Pointer(&langs), &n); err != nil {
		return "", err
	}
	if n < 4 {
		return "", errors.New("the version resource lists no language")
	}
	pair := unsafe.Slice((*uint16)(langs), 2)
	var name unsafe.Pointer
	if err := windows.VerQueryValue(unsafe.Pointer(&info[0]), fmt.Sprintf(`\StringFileInfo\%04x%04x\ProductName`, pair[0], pair[1]), unsafe.Pointer(&name), &n); err != nil {
		return "", err
	}
	// Inno Setup patches its version strings into the setup loader in fixed-width slots padded with spaces, so a real June installer reads "June" and then a run of blanks.
	return strings.TrimSpace(windows.UTF16PtrToString((*uint16)(name))), nil
}

// signedBy checks path's Authenticode signature and reads who made it. Output: the publisher, or why the file does not count as signed: no signature (TRUST_E_NOSIGNATURE), one that does not match the file, or a certificate Windows does not trust for code signing.
func signedBy(path string) (publisher, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return publisher{}, err
	}
	if err := verifyTrust(p); err != nil {
		return publisher{}, err
	}
	return signer(p)
}

// verifyTrust asks Windows whether a file's signature is intact and chains to a root it trusts, with no UI. Revocation is not checked: an unreachable revocation server would then fail every update behind a strict proxy, and a revoked key is a second compromise on top of a tampered release.
func verifyTrust(path *uint16) error {
	data := &windows.WinTrustData{
		Size:             uint32(unsafe.Sizeof(windows.WinTrustData{})),
		UIChoice:         windows.WTD_UI_NONE,
		RevocationChecks: windows.WTD_REVOKE_NONE,
		UnionChoice:      windows.WTD_CHOICE_FILE,
		StateAction:      windows.WTD_STATEACTION_VERIFY,
		FileOrCatalogOrBlobOrSgnrOrCert: unsafe.Pointer(&windows.WinTrustFileInfo{
			Size:     uint32(unsafe.Sizeof(windows.WinTrustFileInfo{})),
			FilePath: path,
		}),
	}
	err := windows.WinVerifyTrustEx(windows.InvalidHWND, &windows.WINTRUST_ACTION_GENERIC_VERIFY_V2, data)
	data.StateAction = windows.WTD_STATEACTION_CLOSE
	windows.WinVerifyTrustEx(windows.InvalidHWND, &windows.WINTRUST_ACTION_GENERIC_VERIFY_V2, data)
	return err
}

// signer reads the certificate of the signature embedded in a file, the one verifyTrust checked, its issuer and the root its chain ends in. Output: the publisher, or the error.
func signer(path *uint16) (publisher, error) {
	var encoding uint32
	var store, msg windows.Handle
	err := windows.CryptQueryObject(windows.CERT_QUERY_OBJECT_FILE, unsafe.Pointer(path), windows.CERT_QUERY_CONTENT_FLAG_PKCS7_SIGNED_EMBED, windows.CERT_QUERY_FORMAT_FLAG_BINARY, 0, &encoding, nil, nil, &store, &msg, nil)
	if err != nil {
		return publisher{}, err
	}
	defer windows.CertCloseStore(store, 0)
	defer procCryptMsgClose.Call(uintptr(msg))
	var size uint32
	if ok, _, err := procCryptMsgGetParam.Call(uintptr(msg), cmsgSignerCertInfoParam, 0, 0, uintptr(unsafe.Pointer(&size))); ok == 0 {
		return publisher{}, err
	}
	info := make([]byte, size)
	if ok, _, err := procCryptMsgGetParam.Call(uintptr(msg), cmsgSignerCertInfoParam, 0, uintptr(unsafe.Pointer(&info[0])), uintptr(unsafe.Pointer(&size))); ok == 0 {
		return publisher{}, err
	}
	cert, err := windows.CertFindCertificateInStore(store, encoding, 0, windows.CERT_FIND_SUBJECT_CERT, unsafe.Pointer(&info[0]), nil)
	if err != nil {
		return publisher{}, err
	}
	defer windows.CertFreeCertificateContext(cert)
	root, err := rootOf(cert, store)
	if err != nil {
		return publisher{}, err
	}
	subject, issuer := distinguishedName(cert, 0), distinguishedName(cert, windows.CERT_NAME_ISSUER_FLAG)
	if subject == "" || issuer == "" {
		return publisher{}, errors.New("the signing certificate has no subject or no issuer")
	}
	return publisher{subject: subject, issuer: issuer, root: root, cn: nameAttr(cert, oidCommonName), org: nameAttr(cert, oidOrganization)}, nil
}

// rootOf builds cert's chain, using the certificates the signature carries to bridge to a root, and names the root it ends in. verifyTrust has just built the same chain and trusted its root, so the certificates it needs are at hand. Input: the certificate and the signature's store. Output: the root's whole subject, or the error.
func rootOf(cert *windows.CertContext, carried windows.Handle) (string, error) {
	para := windows.CertChainPara{Size: uint32(unsafe.Sizeof(windows.CertChainPara{}))}
	var chain *windows.CertChainContext
	if err := windows.CertGetCertificateChain(0, cert, nil, carried, &para, 0, 0, &chain); err != nil {
		return "", err
	}
	defer windows.CertFreeCertificateChain(chain)
	if chain.ChainCount == 0 {
		return "", errors.New("the signing certificate has no chain")
	}
	simple := unsafe.Slice(chain.Chains, chain.ChainCount)[0]
	if simple.NumElements == 0 || simple.TrustStatus.ErrorStatus&windows.CERT_TRUST_IS_PARTIAL_CHAIN != 0 {
		return "", errors.New("the signing certificate's chain does not reach a root")
	}
	elems := unsafe.Slice(simple.Elements, simple.NumElements)
	root := distinguishedName(elems[len(elems)-1].CertContext, 0)
	if root == "" {
		return "", errors.New("the root certificate has no subject")
	}
	return root, nil
}

// distinguishedName is a certificate's whole subject, or with CERT_NAME_ISSUER_FLAG its issuer's; "" when it cannot be read.
func distinguishedName(cert *windows.CertContext, flags uint32) string {
	strType := uint32(certX500NameStr)
	n := windows.CertGetNameString(cert, windows.CERT_NAME_RDN_TYPE, flags, unsafe.Pointer(&strType), nil, 0)
	if n <= 1 {
		return ""
	}
	buf := make([]uint16, n)
	windows.CertGetNameString(cert, windows.CERT_NAME_RDN_TYPE, flags, unsafe.Pointer(&strType), &buf[0], n)
	return windows.UTF16ToString(buf)
}

// nameAttr reads one attribute of a certificate's subject, "" when it has none.
func nameAttr(cert *windows.CertContext, oid string) string {
	o := append([]byte(oid), 0)
	n := windows.CertGetNameString(cert, windows.CERT_NAME_ATTR_TYPE, 0, unsafe.Pointer(&o[0]), nil, 0)
	if n <= 1 {
		return ""
	}
	buf := make([]uint16, n)
	windows.CertGetNameString(cert, windows.CERT_NAME_ATTR_TYPE, 0, unsafe.Pointer(&o[0]), &buf[0], n)
	return windows.UTF16ToString(buf)
}
