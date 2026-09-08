//go:build !darwin

package workerupdate

import (
	"context"
	"errors"
)

var errPackageInstall = errors.New("package installation is unsupported on this platform")

func ExtractDarwinPackage(context.Context, string, string) (string, error) {
	return "", errPackageInstall
}
