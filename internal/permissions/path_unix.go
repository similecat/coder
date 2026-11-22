//go:build !windows

package permissions

import "path/filepath"

func evalRealPath(p string) (string, error) {
	return filepath.EvalSymlinks(p)
}
