//go:build !windows

package filetransfer

func diskFullPlatform(error) bool { return false }
