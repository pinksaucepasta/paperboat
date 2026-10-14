//go:build !darwin

package workerupdate

import (
	"context"
)

func ExtractDarwinPackage(context.Context, string, string) (string, error) {
	return "", errPackageInstall
}
