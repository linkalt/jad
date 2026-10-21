//go:build !linux

package app

func statFile(path string) (fileStat, error) { return statFileGeneric(path) }
