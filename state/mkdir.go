package state

import "os"

func mkdirAll(p string, mode os.FileMode) error { return os.MkdirAll(p, mode) }
