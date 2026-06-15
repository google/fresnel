//go:build !windows
// +build !windows

package installer

// runAsUser executes the given function. On non-Windows platforms, it does not
// perform any impersonation.
func (i *Installer) runAsUser(f func() error) error {
	return f()
}
