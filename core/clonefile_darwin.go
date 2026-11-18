package core

import (
	"context"
	"fmt"
	"os/exec"
	"time"
)

// cloneTimeout bounds one copy. macOS can hold an open of a file on an
// external volume until a privacy prompt is answered, which may never
// happen for a background service.
var cloneTimeout = 2 * time.Minute

// cloneFile makes dst a copy-on-write clone of src (APFS), which takes no
// time or space; across volumes cp makes a plain copy. It runs in its own
// process so that a copy the system holds can be stopped: that fails with
// errCopyTimeout, and callers must not retry the copy themselves.
func cloneFile(src, dst string) error {
	ctx, cancel := context.WithTimeout(context.Background(), cloneTimeout)
	defer cancel()
	err := exec.CommandContext(ctx, "/bin/cp", "-c", src, dst).Run()
	if err != nil && ctx.Err() != nil {
		return fmt.Errorf("%w after %s (macOS may be waiting for a privacy approval to read %s)", errCopyTimeout, cloneTimeout, src)
	}
	return err
}
