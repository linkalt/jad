//go:build !unix

package remote

import "os"

func checkSocketOwner(string, os.FileInfo) error { return nil }
