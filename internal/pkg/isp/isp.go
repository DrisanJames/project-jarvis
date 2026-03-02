package isp

import "strings"

// Known ISP group names returned by Group and GroupFromDomain.
const (
	Gmail     = "gmail"
	Yahoo     = "yahoo"
	Microsoft = "microsoft"
	Apple     = "apple"
	Comcast   = "comcast"
	Charter   = "charter"
	ATT       = "att"
	Cox       = "cox"
	Other     = "other"
)

var domainToISP = map[string]string{
	"gmail.com":       Gmail,
	"googlemail.com":  Gmail,
	"yahoo.com":       Yahoo,
	"ymail.com":       Yahoo,
	"aol.com":         Yahoo,
	"att.net":         ATT,
	"sbcglobal.net":   ATT,
	"bellsouth.net":   ATT,
	"outlook.com":     Microsoft,
	"hotmail.com":     Microsoft,
	"live.com":        Microsoft,
	"msn.com":         Microsoft,
	"icloud.com":      Apple,
	"me.com":          Apple,
	"mac.com":         Apple,
	"comcast.net":     Comcast,
	"xfinity.com":     Comcast,
	"charter.net":     Charter,
	"spectrum.net":    Charter,
	"cox.net":         Cox,
}

// Group returns the ISP group name for an email address.
// Returns "other" for unrecognized domains or malformed addresses.
func Group(email string) string {
	at := strings.LastIndexByte(email, '@')
	if at < 0 || at == len(email)-1 {
		return Other
	}
	return GroupFromDomain(email[at+1:])
}

// GroupFromDomain returns the ISP group name for a bare domain.
func GroupFromDomain(domain string) string {
	if g, ok := domainToISP[strings.ToLower(strings.TrimSpace(domain))]; ok {
		return g
	}
	return Other
}

// KnownGroups returns the list of recognized ISP group names.
func KnownGroups() []string {
	return []string{Gmail, Yahoo, Microsoft, Apple, Comcast, Charter, ATT, Cox}
}
