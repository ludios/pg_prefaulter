package fhcache

/*
#include <fcntl.h>
*/
import "C"

// Fadvise is a wrapper around posix_fadvise()
func Fadvise(fd int, off, len int64) {
	// We advise the kernel that we are performing likely-random access
	C.posix_fadvise(C.int(fd), C.__off_t(off), C.__off_t(len), C.int(C.POSIX_FADV_WILLNEED|C.POSIX_FADV_RANDOM))
}
