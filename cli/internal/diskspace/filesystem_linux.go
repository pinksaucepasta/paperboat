package diskspace

import "syscall"

// msdosSuperMagic is statfs's f_type for FAT12, FAT16, FAT32 and vfat (linux/magic.h MSDOS_SUPER_MAGIC).
const msdosSuperMagic = 0x4d44

func fileSystem(path string) (string, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return "", err
	}
	if int64(stat.Type) == msdosSuperMagic {
		return "FAT", nil
	}
	return "other", nil
}
