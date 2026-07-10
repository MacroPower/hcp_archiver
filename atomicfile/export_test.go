package atomicfile

import "io/fs"

// MkdirAllSync exposes mkdirAllSync to the external test package, letting a test
// inject a recording directory-sync function to observe which directories are
// flushed.
func MkdirAllSync(dir string, mode fs.FileMode, sync func(string) error) error {
	return mkdirAllSync(dir, mode, sync)
}
