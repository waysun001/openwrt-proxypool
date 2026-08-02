//go:build windows

package diagnostics

import "os"

func ownedByEffectiveUser(os.FileInfo) bool { return true }
