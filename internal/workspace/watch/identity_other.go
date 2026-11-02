//go:build !unix && !windows

package watch

import "os"

// Platforms without a reachable file identity keep the legacy stat-signature
// behavior: identityStable degrades to true and the (mtime,size) short-circuit
// stays authoritative (the same tradeoff every stat-based poller makes).
func statIdentity(string, os.FileInfo) (fileID, bool) { return fileID{}, false }
