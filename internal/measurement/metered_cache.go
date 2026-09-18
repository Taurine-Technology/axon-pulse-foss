//go:build linux || windows

package measurement

import (
	"sync"
	"time"
)

const (
	meteredRefreshInterval = 5 * time.Minute
)

// meteredCache bounds metered-state detection to one subprocess launch per
// association or refresh interval: both the Linux (nmcli) and Windows
// (powershell + WinRT) probes are far too expensive to fork on every periodic
// link-context collection.
var (
	meteredCache = struct {
		mu        sync.Mutex
		key       string
		fetched   time.Time
		metered   bool
		available bool
	}{}
)

func cachedMetered(now time.Time, key string, fetch func() (bool, bool)) (bool, bool) {
	meteredCache.mu.Lock()
	defer meteredCache.mu.Unlock()
	if meteredCache.key == key && !meteredCache.fetched.IsZero() && now.Sub(meteredCache.fetched) < meteredRefreshInterval {
		return meteredCache.metered, meteredCache.available
	}
	metered, available := fetch()
	meteredCache.key, meteredCache.fetched = key, now
	meteredCache.metered, meteredCache.available = metered, available
	return metered, available
}
