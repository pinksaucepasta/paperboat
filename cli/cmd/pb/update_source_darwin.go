package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/pinksaucepasta/paperboat/internal/hostruntime/workerupdate"
)

func manualUpdateExecutable(ctx context.Context, input, directory string) (string, error) {
	if !strings.EqualFold(filepath.Ext(input), ".pkg") {
		return input, nil
	}
	extraction, err := os.MkdirTemp(directory, "package-")
	if err != nil {
		return "", err
	}
	return workerupdate.ExtractDarwinPackage(ctx, input, extraction)
}
