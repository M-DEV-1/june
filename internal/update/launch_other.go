//go:build !windows

package update

import "errors"

// canSelfInstall is false off Windows: the Linux release is a tarball that install.sh unpacks, and its banner links to the release page.
func canSelfInstall(string) bool { return false }

// startInstaller is never reached off Windows, since canSelfInstall is false there.
func startInstaller(string, string) (func() error, error) {
	return nil, errors.New("June installs its own updates only on Windows")
}

// publisher, ownPublisher and samePublisher are never reached off Windows either; there is no Authenticode signature to hold an installer to.
type publisher struct{}

func ownPublisher() (*publisher, error) { return nil, nil }

func samePublisher(string, *publisher) error { return nil }
