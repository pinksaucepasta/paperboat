// Command mkwinres writes the Windows resource object linked into pb.exe: the long-path manifest and
// the version information that Windows shows in permission and system dialogs ("Paperboat", not "pb").
// Reference: tailscale cmd/mkmanifest (winres), netbird release.yml goversioninfo step.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/tc-hib/winres"
	"github.com/tc-hib/winres/version"
)

const product = "Paperboat"

var numericVersion = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,3}$`)

func resources(release string) (*winres.ResourceSet, error) {
	var vi version.Info
	// Dev builds ("dev-abc123") have no numeric version; the string fields still carry it.
	fileVersion := "0.0.0.0"
	if numericVersion.MatchString(release) {
		fileVersion = release
	}
	vi.SetFileVersion(fileVersion)
	vi.SetProductVersion(fileVersion)
	for key, value := range map[string]string{
		version.FileDescription:  product,
		version.ProductName:      product,
		version.CompanyName:      product,
		version.InternalName:     "pb",
		version.OriginalFilename: "pb.exe",
		version.ProductVersion:   release,
	} {
		if err := vi.Set(version.LangDefault, key, value); err != nil {
			return nil, fmt.Errorf("set version field %s: %w", key, err)
		}
	}
	rs := &winres.ResourceSet{}
	rs.SetManifest(winres.AppManifest{LongPathAware: true})
	rs.SetVersionInfo(vi)
	return rs, nil
}

func write(w io.Writer, release string, arch winres.Arch) error {
	rs, err := resources(release)
	if err != nil {
		return err
	}
	return rs.WriteObject(w, arch)
}

func main() {
	release := flag.String("version", "", "release version, e.g. 2026.11.09.7 (empty: 0.0.0.0)")
	arch := flag.String("arch", "", "amd64 or arm64")
	out := flag.String("out", "", "output .syso path")
	flag.Parse()
	archs := map[string]winres.Arch{"amd64": winres.ArchAMD64, "arm64": winres.ArchARM64}
	a, ok := archs[*arch]
	if !ok || *out == "" {
		fmt.Fprintln(os.Stderr, "usage: mkwinres -arch amd64|arm64 -out FILE [-version VERSION]")
		os.Exit(2)
	}
	f, err := os.Create(*out)
	if err == nil {
		err = write(f, *release, a)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "mkwinres:", err)
		os.Exit(1)
	}
}
