package isp

import "sort"

// DomainSiblings maps an @-prefixed email domain (e.g. "@outlook.com") to
// every domain in the same ISP group, also @-prefixed. Auto-derived from
// domainToISP so the two can never diverge.
//
// Segment query builders use this to expand a condition like
// "email contains @outlook.com" into an OR across all Microsoft domains.
var DomainSiblings map[string][]string

func init() {
	groups := map[string][]string{}
	for domain, ispGroup := range domainToISP {
		groups[ispGroup] = append(groups[ispGroup], "@"+domain)
	}
	for _, siblings := range groups {
		sort.Strings(siblings)
	}

	DomainSiblings = make(map[string][]string, len(domainToISP))
	for domain, ispGroup := range domainToISP {
		DomainSiblings["@"+domain] = groups[ispGroup]
	}
}
