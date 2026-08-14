package atomicfile

import "io/fs"

// MkdirAllSync exposes mkdirAllSync to the external test package, letting a test
// inject a recording directory-sync function to observe which directories are
// flushed.
func MkdirAllSync(dir string, mode fs.FileMode, sync func(string) error) error {
	return mkdirAllSync(dir, mode, sync)
}

// AppendSync exposes appendSync to the external test package, letting a test
// fail or record the directory flush that guards a first append.
func AppendSync(name string, data []byte, sync func(string) error, opts ...Option) (int64, error) {
	return appendSync(name, data, sync, opts...)
}
