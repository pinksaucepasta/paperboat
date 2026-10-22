package installsource

import "syscall"

func statIdentity(s *syscall.Stat_t) fileIdentity {
	return fileIdentity{device: uint64(s.Dev), inode: s.Ino, size: s.Size,
		mtimeSeconds: s.Mtimespec.Sec, mtimeNanoseconds: s.Mtimespec.Nsec, ctimeSeconds: s.Ctimespec.Sec, ctimeNanoseconds: s.Ctimespec.Nsec}
}
