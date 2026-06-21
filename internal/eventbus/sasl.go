package eventbus

import (
	"context"
	"crypto/tls"
	"fmt"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/twmb/franz-go/pkg/kgo"
	"github.com/twmb/franz-go/pkg/sasl/aws"
)

// authOpts returns the franz-go client options for the configured auth/TLS
// posture. This is the wiring layer the dark foundation deferred (producer.go /
// config.go): it imports the AWS SASL package + credential chain here, in ONE
// place shared by the producer and the consumer, so both authenticate identically.
//
//   - "" / "none": plaintext (dev), or TLS-only if cfg.TLS.
//   - "aws_msk_iam": AWS MSK IAM-SASL over TLS. Credentials resolve from the
//     DEFAULT AWS chain — in prod that is the ECS task role (no static secrets).
//     franz-go derives the signing region from the MSK bootstrap broker host.
func authOpts(cfg Config) ([]kgo.Opt, error) {
	switch cfg.SASLMechanism {
	case "", "none":
		if cfg.TLS {
			return []kgo.Opt{kgo.DialTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12})}, nil
		}
		return nil, nil
	case "aws_msk_iam":
		// MSK IAM-SASL mandates TLS.
		return []kgo.Opt{
			kgo.DialTLSConfig(&tls.Config{MinVersion: tls.VersionTLS12}),
			kgo.SASL(aws.ManagedStreamingIAM(func(ctx context.Context) (aws.Auth, error) {
				ac, err := awsconfig.LoadDefaultConfig(ctx)
				if err != nil {
					return aws.Auth{}, fmt.Errorf("eventbus: load AWS config: %w", err)
				}
				creds, err := ac.Credentials.Retrieve(ctx)
				if err != nil {
					return aws.Auth{}, fmt.Errorf("eventbus: retrieve AWS credentials: %w", err)
				}
				return aws.Auth{
					AccessKey:    creds.AccessKeyID,
					SecretKey:    creds.SecretAccessKey,
					SessionToken: creds.SessionToken,
				}, nil
			})),
		}, nil
	default:
		return nil, fmt.Errorf("eventbus: unsupported KAFKA_SASL_MECHANISM %q", cfg.SASLMechanism)
	}
}
