package reed

import (
	"context"
	"net"
)

// SetLookupIP replaces how act sessions resolve the names they pin, and
// returns what restores it.
func SetLookupIP(f func(context.Context, string) ([]net.IPAddr, error)) (restore func()) {
	old := lookupIP
	lookupIP = f
	return func() { lookupIP = old }
}
