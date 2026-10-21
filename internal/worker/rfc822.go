package worker

import (
	"bytes"
	"fmt"
	"sort"
)

// BuildRFC822 assembles the wire message the PMTA / KumoMTA HTTP bridge
// injects: the headers PMTAAPISender always writes, then msg.Headers in
// SORTED key order (deterministic; the previous inline builder iterated the
// map in random order, which was harmless on the wire but made the bytes
// unreproducible), then a multipart/alternative body with the text/plain
// part first when present and the text/html part always, both
// quoted-printable.
//
// messageID and boundary are injected so that (a) the sender keeps its
// per-send random values and (b) the render-only Inbox Score path can pin
// them and produce byte-identical output for the same message twice.
func BuildRFC822(msg *EmailMessage, messageID, boundary string) string {
	var rfc822 bytes.Buffer

	rfc822.WriteString(BuildFromHeader(msg.FromName, msg.FromEmail))
	rfc822.WriteString(fmt.Sprintf("To: %s\r\n", msg.Email))
	rfc822.WriteString(BuildSubjectHeader(msg.Subject))
	rfc822.WriteString(fmt.Sprintf("Message-ID: <%s>\r\n", messageID))
	rfc822.WriteString("MIME-Version: 1.0\r\n")

	if msg.ReplyTo != "" {
		rfc822.WriteString(fmt.Sprintf("Reply-To: %s\r\n", msg.ReplyTo))
	}

	if msg.CampaignID != "" {
		rfc822.WriteString(fmt.Sprintf("X-Campaign-ID: %s\r\n", msg.CampaignID))
	}
	if msg.SubscriberID != "" {
		rfc822.WriteString(fmt.Sprintf("X-Subscriber-ID: %s\r\n", msg.SubscriberID))
	}
	if msg.ID != "" {
		rfc822.WriteString(fmt.Sprintf("X-Message-ID: %s\r\n", msg.ID))
	}

	keys := make([]string, 0, len(msg.Headers))
	for k := range msg.Headers {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		rfc822.WriteString(fmt.Sprintf("%s: %s\r\n", k, msg.Headers[k]))
	}

	rfc822.WriteString(fmt.Sprintf("Content-Type: multipart/alternative; boundary=\"%s\"\r\n", boundary))
	rfc822.WriteString("\r\n")

	if msg.TextContent != "" {
		rfc822.WriteString(fmt.Sprintf("--%s\r\n", boundary))
		rfc822.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
		rfc822.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		rfc822.WriteString(quotedprintableEncode(msg.TextContent))
		rfc822.WriteString("\r\n")
	}
	rfc822.WriteString(fmt.Sprintf("--%s\r\n", boundary))
	rfc822.WriteString("Content-Type: text/html; charset=UTF-8\r\n")
	rfc822.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	rfc822.WriteString(quotedprintableEncode(msg.HTMLContent))
	rfc822.WriteString("\r\n")
	rfc822.WriteString(fmt.Sprintf("--%s--\r\n", boundary))
	return rfc822.String()
}
