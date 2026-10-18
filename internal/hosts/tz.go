package hosts

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// localtimeLink is read for the zone name when TZ is unset (a var for tests).
var localtimeLink = "/etc/localtime"

// DisplayZone is this machine's display time zone as an IANA name, which a
// forwarded command passes as TZ so the host renders times in it: $TZ when
// it names a zone, else the zoneinfo/ part of /etc/localtime's link target
// (Linux: /usr/share/zoneinfo/Europe/Istanbul, macOS:
// /var/db/timezone/zoneinfo/Europe/Istanbul). "" when it cannot be named
// (a copied /etc/localtime, a POSIX TZ rule).
func DisplayZone() string {
	if tz, ok := os.LookupEnv("TZ"); ok {
		tz = strings.TrimPrefix(tz, ":")
		if tz == "" {
			return "UTC" // TZ set but empty means UTC (tzset)
		}
		if filepath.IsAbs(tz) {
			return zoneFromPath(tz)
		}
		if _, err := time.LoadLocation(tz); err == nil {
			return tz
		}
		return ""
	}
	return zoneFromPath(localtimeLink)
}

func zoneFromPath(p string) string {
	target, err := filepath.EvalSymlinks(p)
	if err != nil {
		return ""
	}
	i := strings.LastIndex(target, "zoneinfo/")
	if i < 0 {
		return ""
	}
	name := target[i+len("zoneinfo/"):]
	// zoneinfo/posix/Europe/Istanbul and zoneinfo/right/… are copies.
	name = strings.TrimPrefix(strings.TrimPrefix(name, "posix/"), "right/")
	if _, err := time.LoadLocation(name); err != nil {
		return ""
	}
	return name
}
