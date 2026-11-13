package diskspace

import "golang.org/x/sys/windows"

func fileSystem(path string) (string, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return "", err
	}
	root := make([]uint16, windows.MAX_PATH+1)
	if err := windows.GetVolumePathName(name, &root[0], uint32(len(root))); err != nil {
		return "", err
	}
	var fs [windows.MAX_PATH + 1]uint16
	if err := windows.GetVolumeInformation(&root[0], nil, 0, nil, nil, nil, &fs[0], uint32(len(fs))); err != nil {
		return "", err
	}
	if kind := windows.UTF16ToString(fs[:]); kind == "FAT" || kind == "FAT32" { // exFAT is reported as "exFAT"
		return "FAT", nil
	} else {
		return kind, nil
	}
}
