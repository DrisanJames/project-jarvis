package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ignite/sparkpost-monitor/internal/pkg/brand"
	"github.com/ignite/sparkpost-monitor/internal/preferences"
	"github.com/ignite/sparkpost-monitor/internal/worker"
)

// txnListUnsubKey carries a transactional send's List-Unsubscribe decision
// from HandleSendTransactional into HandleSendTestEmail (the transport it
// reuses). A request-context value, not a JSON field, so no API caller can
// inject a header. Header == "" means "send no List-Unsubscribe".
type txnListUnsubKey struct{}

type txnListUnsub struct {
	Header string
}

// txnUnsubLinks is what a transactional send exposes for its recipient.
type txnUnsubLinks struct {
	PrefsURL string       // {{ system.*unsubscribe_url }} / preferences_url
	Header   txnListUnsub // List-Unsubscribe for the transport
}

// buildTxnUnsubLinks mints the recipient links for a transactional send.
// subscriberID "" (no subscriber row) -> no links and no header: there is
// nothing to unsubscribe from. Otherwise:
//   - in-body links -> the preference page on the profile's tracking host
//     (brand-scoped v1 token; the page is where the choice is made)
//   - List-Unsubscribe https leg -> the API one-click endpoint with the same
//     token. It must be the API host: /unsubscribe/one-click is 403'd on
//     tracking hosts, and the /track/unsubscribe format there resolves scope
//     from a campaign row a transactional send does not have.
//   - mailto leg -> signed org|txn|subscriber, handled by the inbound webhook.
func buildTxnUnsubLinks(orgID, txnID, subscriberID, fromEmail, trackBase, mailtoKey string, now time.Time) txnUnsubLinks {
	if subscriberID == "" {
		return txnUnsubLinks{}
	}
	br := brand.RootFromEmail(fromEmail)
	scope := preferences.ScopeBrand
	if br == "" {
		scope = preferences.ScopeAll
	}
	tok := preferences.Mint(orgID, subscriberID, br, scope, now, preferencesSecret())
	if tok == "" {
		return txnUnsubLinks{}
	}
	out := txnUnsubLinks{}
	if trackBase != "" {
		out.PrefsURL = worker.GeneratePreferencesURL(orgID, subscriberID, br, trackBase, preferencesSecret(), now)
	}
	fromDomain := fromEmail
	if at := strings.LastIndex(fromEmail, "@"); at >= 0 {
		fromDomain = fromEmail[at+1:]
	}
	mailtoData := base64.URLEncoding.EncodeToString([]byte(orgID + "|" + txnID + "|" + subscriberID))
	https := PreferencesBaseURL() + "/unsubscribe/one-click?t=" + url.QueryEscape(tok)
	if fromDomain != "" {
		out.Header.Header = fmt.Sprintf("<mailto:unsub+%s@%s?subject=unsubscribe>, <%s>",
			worker.SignedMailtoToken(mailtoData, mailtoKey), fromDomain, https)
	} else {
		out.Header.Header = "<" + https + ">"
	}
	return out
}

// sendPreferenceLinkEmail is the fresh-link sender for the preference page:
// a one-off transactional mail from brandRoot's default sending profile,
// routed through HandleSendTestEmail like HandleSendTransactional. It carries
// no List-Unsubscribe (it is a requested, non-marketing message).
func (svc *MailingService) sendPreferenceLinkEmail(ctx context.Context, email, brandRoot, link string) error {
	if brandRoot == "" {
		return fmt.Errorf("fresh link: no brand to send from")
	}
	label := brand.Label(brandRoot)
	htmlBody := `<html><body style="font-family:Arial,sans-serif;color:#1f2937">` +
		`<p>Here is your new link to manage your ` + html.EscapeString(label) + ` email preferences:</p>` +
		`<p><a href="` + html.EscapeString(link) + `">Manage email preferences</a></p>` +
		`<p style="color:#6b7280;font-size:12px">A new link was requested from an expired preferences page. ` +
		`If that wasn't you, ignore this email — nothing changes unless you use the link.</p></body></html>`
	body, err := json.Marshal(map[string]interface{}{
		"to":             email,
		"subject":        label + ": your email preferences link",
		"html_content":   htmlBody,
		"text_content":   "Manage your " + label + " email preferences: " + link,
		"sending_domain": "em." + brandRoot,
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(context.WithValue(ctx, txnListUnsubKey{}, txnListUnsub{}),
		http.MethodPost, "/api/mailing/send-test", bytes.NewReader(body))
	if err != nil {
		return err
	}
	rec := &responseRecorder{header: http.Header{}, code: http.StatusOK}
	svc.HandleSendTestEmail(rec, req)
	if rec.code >= 300 {
		return fmt.Errorf("fresh link: send-test status %d: %s", rec.code, strings.TrimSpace(string(rec.body)))
	}
	return nil
}
