// Package mailing — ingest_guard.go
//
// Layer-1 ingest guard. Rejects addresses that should never enter
// mailing_subscribers in the first place. Catches:
//
//   - Typo-trap domains: gmai.com, hotmial.com, yaho.com, etc.
//     These are real, registered domains operated by honeypot networks
//     that harvest typos. Delivering to them trains RBLs to block us.
//
//   - Disposable / temporary mailbox providers: mailinator.com,
//     guerrillamail.com, 10minutemail.com, etc. These produce zero
//     engagement and look like bots to receiving ISPs.
//
//   - Role-based local parts: abuse@, postmaster@, webmaster@,
//     noreply@. These go to shared inboxes or directly to T&S teams.
//
// Not covered here (deliberate, Layer-2 follow-ups):
//   - MX-record existence check. Adds 20-200ms DNS latency per insert
//     and risks false positives during transient DNS failures. Better
//     implemented as an async worker that sweeps recently-added
//     subscribers.
//   - External hygiene API (ZeroBounce / NeverBounce). Requires a
//     vendor contract and budgeted per-check cost.
//
// Source of truth: the three sets below are compiled in. If you need
// to add a domain without redeploying, INSERT into mailing_suppressions
// at the email level or add it to the typo-trap list on next deploy.
// A future iteration can externalise these sets to a mailing_bad_domains
// table if the lists grow unwieldy.
package mailing

import (
	"strings"
)

// IngestDecision is what ClassifyEmailForIngest returns.
// When Accept is false, Reason carries a short machine-readable tag
// that callers can log or surface to the user (e.g. "typo_trap_domain").
type IngestDecision struct {
	Accept   bool
	Reason   string // machine-readable tag
	Category string // human-readable category
}

// Accept is the happy-path decision.
func acceptIngest() IngestDecision {
	return IngestDecision{Accept: true}
}

// reject builds a rejection decision.
func rejectIngest(reason, category string) IngestDecision {
	return IngestDecision{Accept: false, Reason: reason, Category: category}
}

// ClassifyEmailForIngest is the single entry point. Handlers call this
// before writing to mailing_subscribers. Expected to be <1µs per call
// since it's three map lookups and a regex-free split.
//
// Input is expected to be already-lowercased and trimmed by the caller,
// but we defensively lowercase again — cheap and avoids a subtle bug
// where a caller forgets.
func ClassifyEmailForIngest(email string) IngestDecision {
	e := strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndexByte(e, '@')
	if at <= 0 || at == len(e)-1 {
		return rejectIngest("invalid_format", "Email missing local part or domain")
	}
	localPart := e[:at]
	domain := e[at+1:]

	// Role-based local parts are hard-block. Even if the domain is fine,
	// these addresses are operational aliases that we should never mail.
	if _, bad := roleBasedLocalParts[localPart]; bad {
		return rejectIngest("role_based_local_part", "Role-based address (e.g. abuse@, postmaster@)")
	}

	if _, bad := typoTrapDomains[domain]; bad {
		return rejectIngest("typo_trap_domain", "Known typo-trap domain (operates as a honeypot)")
	}

	if _, bad := disposableDomains[domain]; bad {
		return rejectIngest("disposable_domain", "Disposable / temporary mailbox provider")
	}

	return acceptIngest()
}

// IsTypoTrapDomain exposes the typo check for callers that already
// have the domain parsed (e.g. retroactive suppression migrations).
func IsTypoTrapDomain(domain string) bool {
	_, bad := typoTrapDomains[strings.ToLower(domain)]
	return bad
}

// IsDisposableDomain exposes the disposable check.
func IsDisposableDomain(domain string) bool {
	_, bad := disposableDomains[strings.ToLower(domain)]
	return bad
}

// IsRoleBasedLocalPart exposes the role-based check.
func IsRoleBasedLocalPart(localPart string) bool {
	_, bad := roleBasedLocalParts[strings.ToLower(localPart)]
	return bad
}

// TypoTrapDomainList returns a snapshot of the typo-trap domain list,
// for use by retroactive cleanup migrations that need to push the set
// into SQL. Callers must not mutate the returned slice.
func TypoTrapDomainList() []string {
	out := make([]string, 0, len(typoTrapDomains))
	for d := range typoTrapDomains {
		out = append(out, d)
	}
	return out
}

// DisposableDomainList returns a snapshot of the disposable-domain list.
func DisposableDomainList() []string {
	out := make([]string, 0, len(disposableDomains))
	for d := range disposableDomains {
		out = append(out, d)
	}
	return out
}

// RoleBasedLocalPartList returns a snapshot of the role-based local-part list.
func RoleBasedLocalPartList() []string {
	out := make([]string, 0, len(roleBasedLocalParts))
	for lp := range roleBasedLocalParts {
		out = append(out, lp)
	}
	return out
}

// ---------------------------------------------------------------------------
// Data sets. Keep alphabetised for easy auditing.
// ---------------------------------------------------------------------------

// roleBasedLocalParts — shared inboxes and operational aliases.
// abuse@ and postmaster@ go to ISP trust-and-safety teams; mailing
// them is a fast path to getting RBL-listed. The rest are shared
// mailboxes that aggregate complaints from many humans.
var roleBasedLocalParts = map[string]struct{}{
	"abuse":       {},
	"admin":       {},
	"administrator": {},
	"billing":     {},
	"contact":     {},
	"feedback":    {},
	"help":        {},
	"hostmaster": {},
	"info":        {},
	"mail":        {},
	"mailer-daemon": {},
	"marketing":   {},
	"no-reply":    {},
	"noc":         {},
	"noreply":     {},
	"postmaster":  {},
	"root":        {},
	"sales":       {},
	"security":    {},
	"support":     {},
	"sysadmin":    {},
	"unsubscribe": {},
	"webmaster":   {},
}

// typoTrapDomains — registered domains that mimic the big mailbox
// providers. Every one of these is a honeypot operator. This list is
// curated from public deliverability-industry reporting plus the
// 2026-04-23 Email Oversight audit that flagged subscribers in our
// own database.
//
// When adding: confirm the domain is NOT a legitimate mailbox provider
// (some ccTLD look-alikes are real, e.g. gmail.uk does not exist but
// hotmail.co.uk is a real historical Microsoft domain — verify MX
// ownership before adding).
var typoTrapDomains = map[string]struct{}{
	// Gmail typos
	"gmai.com":   {},
	"gmail.co":   {}, // .co is Colombia — not Gmail
	"gmail.cm":   {},
	"gmail.om":   {},
	"gmaill.com": {},
	"gmali.com":  {},
	"gmial.com":  {},
	"gmil.com":   {},
	"gmsil.com":  {},
	"gamail.com": {},
	"gamil.com":  {},
	"gnail.com":  {},
	"gmaul.com":  {},
	"gmaik.com":  {},
	"gmaio.com":  {},
	"gmaip.com":  {},
	"gnail.co":   {},
	"gmail.con":  {},
	"gmail.vom":  {},
	"ggmail.com": {},

	// Yahoo typos
	"yaho.com":    {},
	"yahho.com":   {},
	"yahoo.co":    {},
	"yahoo.con":   {},
	"yahoo.om":    {},
	"yahooo.com":  {},
	"yahoomail.com": {}, // common phishing-adjacent typo
	"yaboo.com":   {},
	"yahool.com":  {},
	"yhaoo.com":   {},
	"yhoo.com":    {},
	"yshoo.com":   {},
	"ymail.co":    {},

	// Hotmail / Outlook / MSN typos
	"hotmai.com":   {},
	"hotmail.co":   {},
	"hotmail.con":  {},
	"hotmail.cm":   {},
	"hotmail.om":   {},
	"hotmial.com":  {},
	"hotmaill.com": {},
	"hotnail.com":  {},
	"hotmial.co":   {},
	"hotmal.com":   {},
	"hotmil.com":   {},
	"hormail.com":  {},
	"outllook.com": {},
	"outloook.com": {},
	"outlok.com":   {},
	"outook.com":   {},
	"oulook.com":   {},
	"outlook.co":   {},
	"outlook.con":  {},
	"msm.com":      {}, // msn typo
	"msn.co":       {},

	// AOL typos
	"aol.co":  {},
	"aol.con": {},
	"aole.com": {},
	"aoll.com": {},
	"aaol.com": {},
	"oal.com":  {},

	// iCloud / me.com typos
	"icoud.com":   {},
	"iclud.com":   {},
	"iclould.com": {},
	"icluod.com":  {},
	"iclod.com":   {},
	"iclould.co":  {},
	"mee.com":     {},
	"mac.co":      {},

	// Comcast / Verizon / ATT typos
	"comcst.net":   {},
	"comcast.com":  {}, // real suffix is .net
	"verison.net":  {},
	"verizion.net": {},
	"verzon.net":   {},
	"att.ne":       {},
	"aat.net":      {},
	"bellsouth.ne": {},

	// SBCGlobal / Sprint typos
	"sbcgobal.net":  {},
	"sbcglbal.net":  {},
	"sbcglobl.net":  {},
	"sbcglobal.ne":  {},
	"sprintmail.co": {},

	// Charter / Spectrum
	"charte.net":    {},
	"chater.net":    {},
	"spectrum.co":   {},
}

// disposableDomains — temporary / throwaway mailbox providers.
// Curated from the well-known disposable-email-domains public lists,
// filtered to the top-used providers that account for the vast majority
// of real-world disposable signups. Omits long-tail providers.
var disposableDomains = map[string]struct{}{
	"10minutemail.com":     {},
	"10minutemail.net":     {},
	"20minutemail.com":     {},
	"33mail.com":           {},
	"anonbox.net":          {},
	"armyspy.com":          {},
	"burnermail.io":        {},
	"byom.de":              {},
	"cuvox.de":             {},
	"dayrep.com":           {},
	"deadaddress.com":      {},
	"discard.email":        {},
	"discardmail.com":      {},
	"discardmail.de":       {},
	"dispostable.com":      {},
	"dodgeit.com":          {},
	"dodgit.com":           {},
	"dropmail.me":          {},
	"easytrashmail.com":    {},
	"einrot.com":           {},
	"email-fake.com":       {},
	"emailondeck.com":      {},
	"emailsensei.com":      {},
	"emailtemporanea.com":  {},
	"emailtemporanea.net":  {},
	"emailtmp.com":         {},
	"fakeinbox.com":        {},
	"fakemail.net":         {},
	"fakemailgenerator.com": {},
	"fastacura.com":        {},
	"filzmail.com":         {},
	"fleckens.hu":          {},
	"getairmail.com":       {},
	"getnada.com":          {},
	"grr.la":               {},
	"guerrillamail.biz":    {},
	"guerrillamail.com":    {},
	"guerrillamail.de":     {},
	"guerrillamail.info":   {},
	"guerrillamail.net":    {},
	"guerrillamail.org":    {},
	"guerrillamailblock.com": {},
	"harakirimail.com":     {},
	"hidemail.de":          {},
	"inboxalias.com":       {},
	"inboxbear.com":        {},
	"incognitomail.com":    {},
	"incognitomail.net":    {},
	"incognitomail.org":    {},
	"instant-mail.de":      {},
	"jetable.com":          {},
	"jetable.fr.nf":        {},
	"jetable.net":          {},
	"jetable.org":          {},
	"jourrapide.com":       {},
	"kurzepost.de":         {},
	"letthemeatspam.com":   {},
	"lroid.com":            {},
	"mail-temporaire.fr":   {},
	"mail.tm":              {},
	"mail7.io":             {},
	"mailbidon.com":        {},
	"mailcatch.com":        {},
	"maildrop.cc":          {},
	"maileater.com":        {},
	"mailexpire.com":       {},
	"mailforspam.com":      {},
	"mailinator.com":       {},
	"mailinator.net":       {},
	"mailinator.org":       {},
	"mailinator2.com":      {},
	"mailmetrash.com":      {},
	"mailnesia.com":        {},
	"mailnull.com":         {},
	"mailpoof.com":         {},
	"mailsac.com":          {},
	"mailtothis.com":       {},
	"mintemail.com":        {},
	"moakt.com":            {},
	"mohmal.com":           {},
	"mt2015.com":           {},
	"mytemp.email":         {},
	"mytrashmail.com":      {},
	"nada.email":           {},
	"nepwk.com":            {},
	"netmails.net":         {},
	"nobulk.com":           {},
	"noclickemail.com":     {},
	"nomail.xl.cx":         {},
	"nomail2me.com":        {},
	"nowmymail.com":        {},
	"nwldx.com":            {},
	"objectmail.com":       {},
	"oneoffemail.com":      {},
	"onewaymail.com":       {},
	"opayq.com":            {},
	"otherinbox.com":       {},
	"ourklips.com":         {},
	"owlymail.com":         {},
	"pjjkp.com":             {},
	"pokemail.net":         {},
	"privatdemail.net":     {},
	"proxymail.eu":         {},
	"punkass.com":          {},
	"putthisinyourspamdatabase.com": {},
	"rcpt.at":              {},
	"receive-mail.com":     {},
	"receiveee.com":        {},
	"recode.me":            {},
	"recursor.net":         {},
	"reliable-mail.com":    {},
	"rhyta.com":            {},
	"rmqkr.net":            {},
	"safe-mail.net":        {},
	"safetymail.info":      {},
	"safetypost.de":        {},
	"saynotospams.com":     {},
	"selfdestructingmail.com": {},
	"sendspamhere.com":     {},
	"shitmail.me":          {},
	"shitmail.org":         {},
	"slopsbox.com":         {},
	"sneakemail.com":       {},
	"snkmail.com":          {},
	"sofort-mail.de":       {},
	"sogetthis.com":        {},
	"solvemail.info":       {},
	"soodonims.com":        {},
	"spam4.me":             {},
	"spamavert.com":        {},
	"spambog.com":          {},
	"spambog.de":           {},
	"spambog.ru":           {},
	"spambox.info":         {},
	"spambox.us":           {},
	"spamcannon.com":       {},
	"spamcannon.net":       {},
	"spamcero.com":         {},
	"spamcon.org":          {},
	"spamcorptastic.com":   {},
	"spamcowboy.com":       {},
	"spamcowboy.net":       {},
	"spamcowboy.org":       {},
	"spamday.com":          {},
	"spamex.com":           {},
	"spamfree.eu":          {},
	"spamfree24.com":       {},
	"spamfree24.de":        {},
	"spamfree24.eu":        {},
	"spamfree24.info":      {},
	"spamfree24.net":       {},
	"spamfree24.org":       {},
	"spamgourmet.com":      {},
	"spamgourmet.net":      {},
	"spamgourmet.org":      {},
	"spamherelots.com":     {},
	"spamhereplease.com":   {},
	"spamhole.com":         {},
	"spamify.com":          {},
	"spaml.com":            {},
	"spamlink.com":         {},
	"spamobox.com":         {},
	"spamsalad.in":         {},
	"spamslicer.com":       {},
	"spamspot.com":         {},
	"spamthis.co.uk":       {},
	"spamthisplease.com":   {},
	"spamtrail.com":        {},
	"suremail.info":        {},
	"teleworm.com":         {},
	"teleworm.us":          {},
	"temp-mail.com":        {},
	"temp-mail.io":         {},
	"temp-mail.org":        {},
	"temp-mail.ru":         {},
	"tempail.com":          {},
	"tempemail.biz":        {},
	"tempemail.co.za":      {},
	"tempemail.com":        {},
	"tempemail.net":        {},
	"tempinbox.co.uk":      {},
	"tempinbox.com":        {},
	"tempmail.eu":          {},
	"tempmail.us":          {},
	"tempmaildemand.com":   {},
	"tempmailer.com":       {},
	"tempmailer.de":        {},
	"tempomail.fr":         {},
	"temporarily.de":       {},
	"temporarioemail.com.br": {},
	"temporaryemail.net":   {},
	"temporaryforwarding.com": {},
	"temporaryinbox.com":   {},
	"temporarymailaddress.com": {},
	"tempsky.com":          {},
	"tempthe.net":          {},
	"thanksnospam.info":    {},
	"thankyou2010.com":     {},
	"thrma.com":            {},
	"throwam.com":          {},
	"throwawayemailaddresses.com": {},
	"tmail.ws":             {},
	"tmailinator.com":      {},
	"tradermail.info":      {},
	"trash-mail.com":       {},
	"trash-mail.de":        {},
	"trash2009.com":        {},
	"trashdevil.com":       {},
	"trashemail.de":        {},
	"trashmail.at":         {},
	"trashmail.com":        {},
	"trashmail.de":         {},
	"trashmail.me":         {},
	"trashmail.net":        {},
	"trashmail.org":        {},
	"trbvm.com":            {},
	"tyldd.com":            {},
	"uggsrock.com":         {},
	"wegwerfmail.de":       {},
	"wegwerfmail.info":     {},
	"wegwerfmail.net":      {},
	"wegwerfmail.org":      {},
	"wh4f.org":             {},
	"whyspam.me":           {},
	"willhackforfood.biz":  {},
	"willselfdestruct.com": {},
	"winemaven.info":       {},
	"wuzup.net":            {},
	"yopmail.com":          {},
	"yopmail.fr":           {},
	"yopmail.net":          {},
	"yuurok.com":           {},
	"zetmail.com":          {},
	"zoemail.com":          {},
	"zoemail.net":          {},
	"zoemail.org":          {},
}
