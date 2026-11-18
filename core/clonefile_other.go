//go:build !darwin

package core

import "errors"

func cloneFile(string, string) error { return errors.New("clone not supported") }
