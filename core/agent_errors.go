package core

import "errors"

// ErrAgentSessionNotFound means resuming failed before a new turn ran.
// Only this failure is safe to retry automatically in a fresh review session.
var ErrAgentSessionNotFound = errors.New("agent session not found")

var errEmptyAgentResponse = errors.New("agent returned an empty response; review was not completed")
