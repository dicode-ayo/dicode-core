package webui

import "strings"

// uaFamily reduces a User-Agent string to a coarse "browser+OS family"
// fingerprint, deliberately dropping version numbers. Versions churn on every
// auto-update, so binding to an exact UA would force a re-login after each
// browser patch; the family is stable across updates while still catching a
// cookie replayed from a different browser or OS.
//
// The parser is intentionally compact rather than a full UA database: it covers
// the mainstream desktop/mobile combinations and falls back to "other" for the
// long tail. "other" compares equal to itself, so an unrecognised-but-consistent
// client is not flagged as drift.
func uaFamily(ua string) string {
	ua = strings.ToLower(ua)
	if ua == "" {
		return ""
	}

	var browser string
	switch {
	// Order matters: Edge/Opera/Brave UAs also contain "chrome"; Chrome UAs
	// contain "safari". Check the more specific tokens first.
	case strings.Contains(ua, "edg/") || strings.Contains(ua, "edge"):
		browser = "edge"
	case strings.Contains(ua, "opr/") || strings.Contains(ua, "opera"):
		browser = "opera"
	case strings.Contains(ua, "firefox") || strings.Contains(ua, "fxios"):
		browser = "firefox"
	case strings.Contains(ua, "chrome") || strings.Contains(ua, "crios") || strings.Contains(ua, "chromium"):
		browser = "chrome"
	case strings.Contains(ua, "safari"):
		browser = "safari"
	default:
		browser = "other"
	}

	var os string
	switch {
	case strings.Contains(ua, "android"):
		os = "android"
	case strings.Contains(ua, "iphone") || strings.Contains(ua, "ipad") || strings.Contains(ua, "ipod"):
		os = "ios"
	case strings.Contains(ua, "windows"):
		os = "windows"
	case strings.Contains(ua, "mac os") || strings.Contains(ua, "macos") || strings.Contains(ua, "macintosh"):
		os = "macos"
	case strings.Contains(ua, "cros"):
		os = "chromeos"
	case strings.Contains(ua, "linux"):
		os = "linux"
	default:
		os = "other"
	}

	return browser + "/" + os
}
