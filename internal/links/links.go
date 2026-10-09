// Package links builds and parses the key link, https://holebridge.app/k#<key>.<application key>,
// which carries a host's 9-symbol key and the deployment's application key after the #
// (docs/security.md, the application key; docs/cli.md, hosting).
package links

import (
	"net/url"
	"strings"

	"github.com/andrewloable/HoleBridge/internal/errs"
	"github.com/andrewloable/HoleBridge/internal/keys"
)

// DefaultBase is the placeholder domain for links until the owner registers one (docs/cli.md).
const DefaultBase = "https://holebridge.app"

// KeyLink returns base + "/k#" + normalizedKey + "." + the application key as 64 lowercase hex digits.
func KeyLink(base, normalizedKey string, appKey [32]byte) string {
	return base + "/k#" + normalizedKey + "." + keys.FormatAppKey(appKey)
}

// ParseKeyLink reads a key link. The key takes the forgiving input rule of internal/keys.Normalize,
// and the application key must be 64 lowercase hex digits. Any other link, including a handoff
// link (/h), returns an error.
func ParseKeyLink(link string) (normalizedKey string, appKey [32]byte, err error) {
	// The url error is dropped on purpose: it quotes the link, and the link holds the key.
	u, perr := url.Parse(link)
	if perr != nil || u.Scheme != "https" || u.Host == "" || u.Path != "/k" || u.RawQuery != "" || u.ForceQuery {
		return "", [32]byte{}, errs.E("HB-KEY-INVALID", "link", nil)
	}
	keyPart, appPart, ok := strings.Cut(u.Fragment, ".")
	if !ok {
		return "", [32]byte{}, errs.E("HB-KEY-INVALID", "link", nil)
	}
	if normalizedKey, err = keys.Normalize(keyPart); err != nil {
		return "", [32]byte{}, err
	}
	for i := 0; i < len(appPart); i++ {
		if 'A' <= appPart[i] && appPart[i] <= 'F' {
			return "", [32]byte{}, errs.E("HB-APPKEY-INVALID", "uppercase", nil)
		}
	}
	if appKey, err = keys.ParseAppKey(appPart); err != nil {
		return "", [32]byte{}, err
	}
	return normalizedKey, appKey, nil
}
