package installsource

import "syscall"

func statIdentity(s *syscall.Stat_t) fileIdentity {
	return fileIdentity{device: uint64(s.Dev), inode: s.Ino, size: s.Size,
		mtimeSeconds: s.Mtim.Sec, mtimeNanoseconds: s.Mtim.Nsec, ctimeSeconds: s.Ctim.Sec, ctimeNanoseconds: s.Ctim.Nsec}
}
