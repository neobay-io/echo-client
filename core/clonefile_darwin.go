package core

import "os/exec"

// cloneFile makes dst a copy-on-write clone of src (APFS), which takes no
// time or space. It fails when the filesystem cannot clone (e.g. dst on
// another volume); callers then copy.
func cloneFile(src, dst string) error {
	return exec.Command("/bin/cp", "-c", src, dst).Run()
}
