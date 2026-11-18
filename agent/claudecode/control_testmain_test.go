package claudecode

import (
	"context"
	"errors"
)

// Tests never start a real Claude process for control requests.
func init() {
	runControlOnly = func(context.Context, *Agent, map[string]any) (map[string]any, error) {
		return nil, errors.New("tests do not start Claude")
	}
}
