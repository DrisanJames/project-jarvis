package mailing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
)

// DeriveSESSMTPPassword converts an AWS IAM secret access key into an
// SES SMTP password for a given region. The algorithm is specified by AWS:
// https://docs.aws.amazon.com/ses/latest/dg/smtp-credentials.html
func DeriveSESSMTPPassword(secretAccessKey, region string) string {
	const (
		date     = "11111111"
		service  = "ses"
		message  = "SendRawEmail"
		terminal = "aws4_request"
		version  = byte(0x04)
	)

	key := sign([]byte("AWS4"+secretAccessKey), []byte(date))
	key = sign(key, []byte(region))
	key = sign(key, []byte(service))
	key = sign(key, []byte(terminal))
	key = sign(key, []byte(message))

	out := make([]byte, 1+len(key))
	out[0] = version
	copy(out[1:], key)

	return base64.StdEncoding.EncodeToString(out)
}

func sign(key, msg []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(msg)
	return h.Sum(nil)
}
