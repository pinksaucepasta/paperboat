//go:build !unix

package nodelifecycle

import "os"

func lockState(string) (*os.File, error) { return nil, ErrControl }
