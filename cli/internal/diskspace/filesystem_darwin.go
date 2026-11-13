package diskspace

import "syscall"

func fileSystem(path string) (string, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return "", err
	}
	name := make([]byte, 0, len(stat.Fstypename))
	for _, c := range stat.Fstypename {
		if c == 0 {
			break
		}
		name = append(name, byte(c))
	}
	if string(name) == "msdos" { // FAT12, FAT16 and FAT32 volumes; ExFAT is "exfat"
		return "FAT", nil
	}
	return string(name), nil
}
