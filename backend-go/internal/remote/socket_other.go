//go:build !unix

package remote

import (
	"net"
	"os"
)

func checkSocketOwner(string, os.FileInfo) error { return nil }

func checkPeerOwner(string, net.Conn) error { return nil }
