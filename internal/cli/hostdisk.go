package cli

import "syscall"

// diskFreeMB reports the space available on the filesystem holding
// path. It is a seam so status, budget and doctor can be tested without
// a host underneath them.
var diskFreeMB = func(path string) (int, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return int(uint64(st.Bavail) * uint64(st.Bsize) / (1024 * 1024)), nil
}
