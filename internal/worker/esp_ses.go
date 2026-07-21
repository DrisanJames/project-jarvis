package worker

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
	"github.com/ignite/sparkpost-monitor/internal/pkg/logger"
)

// SESSender sends emails via AWS SES using the SDK v2.
type SESSender struct {
	accessKey string
	secretKey string
	region    string
	db        *sql.DB
	client    *sesv2.Client
}

// NewSESSender creates an SES sender. Initializes the AWS SDK client if
// credentials are provided.
func NewSESSender(accessKey, secretKey, region string, db *sql.DB) *SESSender {
	if region == "" {
		region = "us-east-1"
	}

	sender := &SESSender{
		accessKey: accessKey,
		secretKey: secretKey,
		region:    region,
		db:        db,
	}

	if accessKey != "" && secretKey != "" {
		cfg, err := awsconfig.LoadDefaultConfig(context.Background(),
			awsconfig.WithRegion(region),
			awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")),
		)
		if err != nil {
			log.Printf("[SES] Warning: Failed to initialize AWS config: %v", err)
		} else {
			sender.client = sesv2.NewFromConfig(cfg)
		}
	}

	return sender
}

// buildSESEmailInput assembles the SES v2 SendEmail request for a message.
// Factored out of Send so the header mapping is unit-testable without an AWS
// client.
//
// msg.Headers passthrough (added 2026-07-21): the SES "Simple" content type
// silently DROPPED every custom SMTP header — including the RFC 8058
// List-Unsubscribe / List-Unsubscribe-Post pair built by processItem — so all
// vendor_type='ses' profile sends went out without one-click unsubscribe even
// after the 2026-07-14 header fix (Google Postmaster "Not compliant" flag).
// SES v2 supports custom headers on Simple content via Message.Headers; map
// them in deterministic (sorted) order. X-SES-* headers are excluded: those
// are SMTP-interface directives (config set / tenant / message tags) that the
// API expresses as first-class request fields, and SES rejects them as
// content headers.
func buildSESEmailInput(msg *EmailMessage) *sesv2.SendEmailInput {
	input := &sesv2.SendEmailInput{
		FromEmailAddress: aws.String(fmt.Sprintf("%s <%s>", msg.FromName, msg.FromEmail)),
		Destination:      &types.Destination{ToAddresses: []string{msg.Email}},
		Content: &types.EmailContent{
			Simple: &types.Message{
				Subject: &types.Content{Data: aws.String(msg.Subject), Charset: aws.String("UTF-8")},
				Body: &types.Body{
					Html: &types.Content{Data: aws.String(msg.HTMLContent), Charset: aws.String("UTF-8")},
				},
			},
		},
		EmailTags: []types.MessageTag{
			{Name: aws.String("campaign_id"), Value: aws.String(sanitizeSESTagValue(msg.CampaignID))},
			{Name: aws.String("subscriber_id"), Value: aws.String(sanitizeSESTagValue(msg.SubscriberID))},
			// recipient_send_id (outbox queue row id) is the universal join key
			// shared with the PMTA relay path and our tracking pipeline.
			{Name: aws.String("recipient_send_id"), Value: aws.String(sanitizeSESTagValue(msg.ID))},
			{Name: aws.String("isp_group"), Value: aws.String(sanitizeSESTagValue(msg.RecipientISP))},
			{Name: aws.String("route_type"), Value: aws.String("ses_direct")},
		},
	}

	if len(msg.Headers) > 0 {
		keys := make([]string, 0, len(msg.Headers))
		for k := range msg.Headers {
			if strings.HasPrefix(strings.ToUpper(k), "X-SES-") {
				continue
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		hdrs := make([]types.MessageHeader, 0, len(keys))
		for _, k := range keys {
			hdrs = append(hdrs, types.MessageHeader{Name: aws.String(k), Value: aws.String(msg.Headers[k])})
		}
		if len(hdrs) > 0 {
			input.Content.Simple.Headers = hdrs
		}
	}

	if msg.TextContent != "" {
		input.Content.Simple.Body.Text = &types.Content{Data: aws.String(msg.TextContent), Charset: aws.String("UTF-8")}
	}
	if msg.ReplyTo != "" {
		input.ReplyToAddresses = []string{msg.ReplyTo}
	}
	return input
}

// Send delivers a single email through AWS SES.
func (s *SESSender) Send(ctx context.Context, msg *EmailMessage) (*SendResult, error) {
	if s.client == nil {
		return nil, fmt.Errorf("SES client not initialized - check credentials")
	}

	input := buildSESEmailInput(msg)

	result, err := s.client.SendEmail(ctx, input)
	if err != nil {
		log.Printf("[SES] Failed to send to %s: %v", msg.Email, err)
		return &SendResult{Success: false, Error: err, ESPType: "ses"}, nil
	}

	messageID := ""
	if result.MessageId != nil {
		messageID = *result.MessageId
	}

	log.Printf("[SES] Sent to %s (id: %s)", logger.RedactEmail(msg.Email), messageID)

	return &SendResult{
		Success:   true,
		MessageID: messageID,
		ESPType:   "ses",
		SentAt:    time.Now(),
	}, nil
}

// SendBatch sends multiple emails via SES. SES lacks true bulk send, so
// messages are dispatched individually in sequence.
func (s *SESSender) SendBatch(ctx context.Context, messages []EmailMessage) (*BatchSendResult, error) {
	if s.client == nil {
		return nil, fmt.Errorf("SES client not initialized - check credentials")
	}
	if len(messages) == 0 {
		return &BatchSendResult{}, nil
	}
	if len(messages) > 50 {
		return nil, fmt.Errorf("SES batch size %d exceeds max 50", len(messages))
	}

	results := &BatchSendResult{Results: make([]SendResult, len(messages))}
	for i, msg := range messages {
		result, err := s.Send(ctx, &msg)
		if err != nil {
			results.Results[i] = SendResult{Success: false, Error: err, ESPType: "ses"}
			results.Rejected++
		} else {
			results.Results[i] = *result
			if result.Success {
				results.Accepted++
			} else {
				results.Rejected++
			}
		}
	}
	return results, nil
}
