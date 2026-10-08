package hostruntimecmd

import "github.com/pinksaucepasta/paperboat/internal/hostruntime/installsource"

// VerifyInstallSource checks the executable that enrollment will install.
// Download authenticity belongs to the initial installer or the TUF updater.
func VerifyInstallSource() error {
	_, _, err := installsource.Current()
	return err
}
