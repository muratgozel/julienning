package config

import "os"

// WriteFileAtomic writes data via temp file + rename in the same directory,
// so readers never observe a partial file. The directory must exist.
func WriteFileAtomic(path string, data []byte, perm os.FileMode) error {
	return writeAtomic(path, data, perm)
}
