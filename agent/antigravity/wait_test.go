package antigravity

import "time"

// waitTimeout bounds how long a test waits for a fake CLI to finish, an event
// to arrive or a file to appear. It is generous because `go test ./...` runs
// packages in parallel and the fake processes can start slowly under load.
// Passing tests return as soon as the wait is satisfied, so they are not slowed.
const waitTimeout = 30 * time.Second
