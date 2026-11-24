//go:build !windows

package route

import "errors"

func spawnPoolRebuild(string) error { return errors.New("pool rebuild is windows-only") }
