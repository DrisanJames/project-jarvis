package mailing

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/acm"
	acmtypes "github.com/aws/aws-sdk-go-v2/service/acm/types"
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	cftypes "github.com/aws/aws-sdk-go-v2/service/cloudfront/types"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/uuid"
)

// AWSInfrastructureService manages AWS resources for tracking and image CDN
type AWSInfrastructureService struct {
	db           *sql.DB
	acmClient    *acm.Client
	cfClient     *cloudfront.Client
	r53Client    *route53.Client
	s3Client     *s3.Client
	hostedZoneID string // Route53 hosted zone for the tracking domain
	region       string
}

// AWSInfraConfig holds configuration for AWS infrastructure service
type AWSInfraConfig struct {
	Region       string
	HostedZoneID string // Route53 hosted zone ID for DNS management
}

// ACMCertificate represents an ACM certificate
type ACMCertificate struct {
	ID                string              `json:"id"`
	OrgID             string              `json:"org_id"`
	Domain            string              `json:"domain"`
	CertificateARN    string              `json:"certificate_arn"`
	Status            string              `json:"status"`
	ValidationMethod  string              `json:"validation_method"`
	ValidationRecords []ValidationRecord  `json:"validation_records"`
	IssuedAt          *time.Time          `json:"issued_at,omitempty"`
	ExpiresAt         *time.Time          `json:"expires_at,omitempty"`
	CreatedAt         time.Time           `json:"created_at"`
}

// ValidationRecord represents a DNS validation record for ACM
type ValidationRecord struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Value  string `json:"value"`
	Status string `json:"status"`
}

// CloudFrontDist represents a CloudFront distribution
type CloudFrontDist struct {
	ID               string    `json:"id"`
	OrgID            string    `json:"org_id"`
	DistributionType string    `json:"distribution_type"`
	Domain           string    `json:"domain"`
	CloudFrontID     string    `json:"cloudfront_id"`
	CloudFrontDomain string    `json:"cloudfront_domain"`
	OriginDomain     string    `json:"origin_domain"`
	ACMCertARN       string    `json:"acm_cert_arn,omitempty"`
	Status           string    `json:"status"`
	Enabled          bool      `json:"enabled"`
	PriceClass       string    `json:"price_class"`
	CreatedAt        time.Time `json:"created_at"`
}

// S3Bucket represents an S3 bucket
type S3Bucket struct {
	ID                  string    `json:"id"`
	OrgID               string    `json:"org_id"`
	BucketName          string    `json:"bucket_name"`
	BucketType          string    `json:"bucket_type"`
	Region              string    `json:"region"`
	PublicAccessBlocked bool      `json:"public_access_blocked"`
	VersioningEnabled   bool      `json:"versioning_enabled"`
	CreatedAt           time.Time `json:"created_at"`
}

// Route53Record represents a Route53 DNS record
type Route53Record struct {
	ID           string    `json:"id"`
	OrgID        string    `json:"org_id"`
	HostedZoneID string    `json:"hosted_zone_id"`
	RecordName   string    `json:"record_name"`
	RecordType   string    `json:"record_type"`
	RecordValue  string    `json:"record_value"`
	TTL          int       `json:"ttl"`
	ChangeID     string    `json:"change_id,omitempty"`
	Status       string    `json:"status"`
	ResourceType string    `json:"resource_type,omitempty"`
	ResourceID   string    `json:"resource_id,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

// ProvisioningLog represents a log entry for AWS provisioning operations
type ProvisioningLog struct {
	ID             string          `json:"id"`
	ResourceType   string          `json:"resource_type"`
	ResourceID     string          `json:"resource_id"`
	Action         string          `json:"action"`
	AWSResourceID  string          `json:"aws_resource_id,omitempty"`
	AWSResourceARN string          `json:"aws_resource_arn,omitempty"`
	Status         string          `json:"status"`
	RequestParams  json.RawMessage `json:"request_params,omitempty"`
	ResponseData   json.RawMessage `json:"response_data,omitempty"`
	ErrorMessage   string          `json:"error_message,omitempty"`
	StartedAt      time.Time       `json:"started_at"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
}

// NewAWSInfrastructureService creates a new AWS infrastructure service
func NewAWSInfrastructureService(ctx context.Context, db *sql.DB, cfg AWSInfraConfig) (*AWSInfrastructureService, error) {
	// Load AWS config from default profile
	awsCfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	// ACM and CloudFront must use us-east-1 for CloudFront certificates
	usEast1Cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion("us-east-1"))
	if err != nil {
		return nil, fmt.Errorf("loading us-east-1 AWS config: %w", err)
	}

	return &AWSInfrastructureService{
		db:           db,
		acmClient:    acm.NewFromConfig(usEast1Cfg), // ACM for CloudFront must be in us-east-1
		cfClient:     cloudfront.NewFromConfig(usEast1Cfg),
		r53Client:    route53.NewFromConfig(awsCfg),
		s3Client:     s3.NewFromConfig(awsCfg),
		hostedZoneID: cfg.HostedZoneID,
		region:       cfg.Region,
	}, nil
}

// SetClients allows setting AWS clients manually (for testing or custom configuration)
func (s *AWSInfrastructureService) SetClients(acmClient *acm.Client, cfClient *cloudfront.Client, r53Client *route53.Client, s3Client *s3.Client) {
	s.acmClient = acmClient
	s.cfClient = cfClient
	s.r53Client = r53Client
	s.s3Client = s3Client
}

// ============================================
// ACM CERTIFICATE MANAGEMENT
// ============================================

// RequestCertificate requests a new ACM certificate for a domain
func (s *AWSInfrastructureService) RequestCertificate(ctx context.Context, orgID, domain string) (*ACMCertificate, error) {
	// Log the provisioning start
	logID, err := s.logProvisioningStart(ctx, "acm", orgID, "create", map[string]string{"domain": domain})
	if err != nil {
		log.Printf("Failed to log provisioning start: %v", err)
	}

	// Request certificate from ACM
	input := &acm.RequestCertificateInput{
		DomainName:       aws.String(domain),
		ValidationMethod: acmtypes.ValidationMethodDns,
		Tags: []acmtypes.Tag{
			{Key: aws.String("OrgID"), Value: aws.String(orgID)},
			{Key: aws.String("ManagedBy"), Value: aws.String("ignite-mailing")},
		},
	}

	output, err := s.acmClient.RequestCertificate(ctx, input)
	if err != nil {
		s.logProvisioningError(ctx, logID, err.Error())
		return nil, fmt.Errorf("requesting ACM certificate: %w", err)
	}

	certARN := *output.CertificateArn

	// Get certificate details to retrieve validation records
	descOutput, err := s.acmClient.DescribeCertificate(ctx, &acm.DescribeCertificateInput{
		CertificateArn: aws.String(certARN),
	})
	if err != nil {
		s.logProvisioningError(ctx, logID, err.Error())
		return nil, fmt.Errorf("describing certificate: %w", err)
	}

	// Extract validation records
	var validationRecords []ValidationRecord
	for _, opt := range descOutput.Certificate.DomainValidationOptions {
		if opt.ResourceRecord != nil {
			validationRecords = append(validationRecords, ValidationRecord{
				Name:   aws.ToString(opt.ResourceRecord.Name),
				Type:   string(opt.ResourceRecord.Type),
				Value:  aws.ToString(opt.ResourceRecord.Value),
				Status: string(opt.ValidationStatus),
			})
		}
	}

	// Save to database
	cert := &ACMCertificate{
		ID:                uuid.New().String(),
		OrgID:             orgID,
		Domain:            domain,
		CertificateARN:    certARN,
		Status:            string(descOutput.Certificate.Status),
		ValidationMethod:  "DNS",
		ValidationRecords: validationRecords,
		CreatedAt:         time.Now(),
	}

	validationJSON, _ := json.Marshal(validationRecords)
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mailing_acm_certificates (id, org_id, domain, certificate_arn, status, validation_method, validation_records, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
	`, cert.ID, cert.OrgID, cert.Domain, cert.CertificateARN, cert.Status, cert.ValidationMethod, validationJSON, cert.CreatedAt)
	if err != nil {
		s.logProvisioningError(ctx, logID, err.Error())
		return nil, fmt.Errorf("saving certificate to database: %w", err)
	}

	// Log success
	s.logProvisioningComplete(ctx, logID, certARN, certARN)

	return cert, nil
}

// CreateValidationRecords creates Route53 records for ACM certificate validation
func (s *AWSInfrastructureService) CreateValidationRecords(ctx context.Context, certARN string) error {
	// Get certificate details
	descOutput, err := s.acmClient.DescribeCertificate(ctx, &acm.DescribeCertificateInput{
		CertificateArn: aws.String(certARN),
	})
	if err != nil {
		return fmt.Errorf("describing certificate: %w", err)
	}

	// Create Route53 records for validation
	var changes []r53types.Change
	for _, opt := range descOutput.Certificate.DomainValidationOptions {
		if opt.ResourceRecord != nil {
			changes = append(changes, r53types.Change{
				Action: r53types.ChangeActionUpsert,
				ResourceRecordSet: &r53types.ResourceRecordSet{
					Name: opt.ResourceRecord.Name,
					Type: r53types.RRType(opt.ResourceRecord.Type),
					TTL:  aws.Int64(300),
					ResourceRecords: []r53types.ResourceRecord{
						{Value: opt.ResourceRecord.Value},
					},
				},
			})
		}
	}

	if len(changes) == 0 {
		return fmt.Errorf("no validation records found")
	}

	_, err = s.r53Client.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(s.hostedZoneID),
		ChangeBatch: &r53types.ChangeBatch{
			Changes: changes,
			Comment: aws.String("ACM validation records"),
		},
	})
	if err != nil {
		return fmt.Errorf("creating Route53 validation records: %w", err)
	}

	return nil
}

// CheckCertificateStatus checks the status of an ACM certificate
func (s *AWSInfrastructureService) CheckCertificateStatus(ctx context.Context, certARN string) (string, error) {
	descOutput, err := s.acmClient.DescribeCertificate(ctx, &acm.DescribeCertificateInput{
		CertificateArn: aws.String(certARN),
	})
	if err != nil {
		return "", fmt.Errorf("describing certificate: %w", err)
	}

	status := string(descOutput.Certificate.Status)

	// Update database
	_, err = s.db.ExecContext(ctx, `
		UPDATE mailing_acm_certificates 
		SET status = $1, updated_at = NOW()
		WHERE certificate_arn = $2
	`, status, certARN)
	if err != nil {
		log.Printf("Failed to update certificate status in DB: %v", err)
	}

	return status, nil
}

// WaitForCertificateValidation waits for certificate to be issued (with timeout)
func (s *AWSInfrastructureService) WaitForCertificateValidation(ctx context.Context, certARN string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("timeout waiting for certificate validation")
		case <-ticker.C:
			status, err := s.CheckCertificateStatus(ctx, certARN)
			if err != nil {
				return err
			}
			if status == "ISSUED" {
				return nil
			}
			if status == "FAILED" || status == "VALIDATION_TIMED_OUT" {
				return fmt.Errorf("certificate validation failed with status: %s", status)
			}
			log.Printf("Certificate status: %s, waiting...", status)
		}
	}
}

// ============================================
// CLOUDFRONT DISTRIBUTION MANAGEMENT
// ============================================

// CreateTrackingDistribution creates a CloudFront distribution for tracking
func (s *AWSInfrastructureService) CreateTrackingDistribution(ctx context.Context, orgID, domain, originServer, certARN string) (*CloudFrontDist, error) {
	logID, err := s.logProvisioningStart(ctx, "cloudfront", orgID, "create", map[string]string{
		"domain":       domain,
		"origin":       originServer,
		"type":         "tracking",
	})
	if err != nil {
		log.Printf("Failed to log provisioning start: %v", err)
	}

	callerRef := fmt.Sprintf("tracking-%s-%d", orgID, time.Now().Unix())

	input := &cloudfront.CreateDistributionInput{
		DistributionConfig: &cftypes.DistributionConfig{
			CallerReference: aws.String(callerRef),
			Comment:         aws.String(fmt.Sprintf("Tracking distribution for %s", domain)),
			Enabled:         aws.Bool(true),
			Aliases: &cftypes.Aliases{
				Quantity: aws.Int32(1),
				Items:    []string{domain},
			},
			DefaultCacheBehavior: &cftypes.DefaultCacheBehavior{
				TargetOriginId:       aws.String("tracking-origin"),
				ViewerProtocolPolicy: cftypes.ViewerProtocolPolicyRedirectToHttps,
				AllowedMethods: &cftypes.AllowedMethods{
					Quantity: aws.Int32(7),
					Items: []cftypes.Method{
						cftypes.MethodGet,
						cftypes.MethodHead,
						cftypes.MethodOptions,
						cftypes.MethodPut,
						cftypes.MethodPost,
						cftypes.MethodPatch,
						cftypes.MethodDelete,
					},
				},
				CachePolicyId:          aws.String("4135ea2d-6df8-44a3-9df3-4b5a84be39ad"), // CachingDisabled
				OriginRequestPolicyId:  aws.String("216adef6-5c7f-47e4-b989-5492eafa07d3"), // AllViewer
				Compress:               aws.Bool(true),
			},
			Origins: &cftypes.Origins{
				Quantity: aws.Int32(1),
				Items: []cftypes.Origin{
					{
						Id:         aws.String("tracking-origin"),
						DomainName: aws.String(originServer),
						CustomOriginConfig: &cftypes.CustomOriginConfig{
							HTTPPort:             aws.Int32(80),
							HTTPSPort:            aws.Int32(443),
							OriginProtocolPolicy: cftypes.OriginProtocolPolicyHttpsOnly,
							OriginSslProtocols: &cftypes.OriginSslProtocols{
								Quantity: aws.Int32(1),
								Items:    []cftypes.SslProtocol{cftypes.SslProtocolTLSv12},
							},
						},
					},
				},
			},
			PriceClass: cftypes.PriceClassPriceClass100, // US, Canada, Europe
		},
	}

	// Add SSL certificate if provided
	if certARN != "" {
		input.DistributionConfig.ViewerCertificate = &cftypes.ViewerCertificate{
			ACMCertificateArn:      aws.String(certARN),
			SSLSupportMethod:       cftypes.SSLSupportMethodSniOnly,
			MinimumProtocolVersion: cftypes.MinimumProtocolVersionTLSv122021,
		}
	}

	output, err := s.cfClient.CreateDistribution(ctx, input)
	if err != nil {
		s.logProvisioningError(ctx, logID, err.Error())
		return nil, fmt.Errorf("creating CloudFront distribution: %w", err)
	}

	dist := &CloudFrontDist{
		ID:               uuid.New().String(),
		OrgID:            orgID,
		DistributionType: "tracking",
		Domain:           domain,
		CloudFrontID:     *output.Distribution.Id,
		CloudFrontDomain: *output.Distribution.DomainName,
		OriginDomain:     originServer,
		ACMCertARN:       certARN,
		Status:           aws.ToString(output.Distribution.Status),
		Enabled:          true,
		PriceClass:       "PriceClass_100",
		CreatedAt:        time.Now(),
	}

	// Save to database
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mailing_cloudfront_distributions 
		(id, org_id, distribution_type, domain, cloudfront_id, cloudfront_domain, origin_domain, acm_cert_arn, status, enabled, price_class, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, dist.ID, dist.OrgID, dist.DistributionType, dist.Domain, dist.CloudFrontID, dist.CloudFrontDomain,
		dist.OriginDomain, nullIfEmpty(dist.ACMCertARN), dist.Status, dist.Enabled, dist.PriceClass, dist.CreatedAt)
	if err != nil {
		s.logProvisioningError(ctx, logID, err.Error())
		return nil, fmt.Errorf("saving distribution to database: %w", err)
	}

	s.logProvisioningComplete(ctx, logID, dist.CloudFrontID, "")

	return dist, nil
}

// CreateImageCDNDistribution creates a CloudFront distribution for image CDN
func (s *AWSInfrastructureService) CreateImageCDNDistribution(ctx context.Context, orgID, domain, s3Bucket, certARN string) (*CloudFrontDist, error) {
	logID, err := s.logProvisioningStart(ctx, "cloudfront", orgID, "create", map[string]string{
		"domain":   domain,
		"s3_bucket": s3Bucket,
		"type":     "image_cdn",
	})
	if err != nil {
		log.Printf("Failed to log provisioning start: %v", err)
	}

	callerRef := fmt.Sprintf("imagecdn-%s-%d", orgID, time.Now().Unix())
	s3Origin := fmt.Sprintf("%s.s3.%s.amazonaws.com", s3Bucket, s.region)

	input := &cloudfront.CreateDistributionInput{
		DistributionConfig: &cftypes.DistributionConfig{
			CallerReference: aws.String(callerRef),
			Comment:         aws.String(fmt.Sprintf("Image CDN distribution for %s", domain)),
			Enabled:         aws.Bool(true),
			Aliases: &cftypes.Aliases{
				Quantity: aws.Int32(1),
				Items:    []string{domain},
			},
			DefaultCacheBehavior: &cftypes.DefaultCacheBehavior{
				TargetOriginId:       aws.String("s3-origin"),
				ViewerProtocolPolicy: cftypes.ViewerProtocolPolicyRedirectToHttps,
				AllowedMethods: &cftypes.AllowedMethods{
					Quantity: aws.Int32(2),
					Items: []cftypes.Method{
						cftypes.MethodGet,
						cftypes.MethodHead,
					},
				},
				CachePolicyId: aws.String("658327ea-f89d-4fab-a63d-7e88639e58f6"), // CachingOptimized
				Compress:      aws.Bool(true),
			},
			Origins: &cftypes.Origins{
				Quantity: aws.Int32(1),
				Items: []cftypes.Origin{
					{
						Id:         aws.String("s3-origin"),
						DomainName: aws.String(s3Origin),
						S3OriginConfig: &cftypes.S3OriginConfig{
							OriginAccessIdentity: aws.String(""),
						},
					},
				},
			},
			PriceClass: cftypes.PriceClassPriceClass100,
		},
	}

	// Add SSL certificate if provided
	if certARN != "" {
		input.DistributionConfig.ViewerCertificate = &cftypes.ViewerCertificate{
			ACMCertificateArn:      aws.String(certARN),
			SSLSupportMethod:       cftypes.SSLSupportMethodSniOnly,
			MinimumProtocolVersion: cftypes.MinimumProtocolVersionTLSv122021,
		}
	}

	output, err := s.cfClient.CreateDistribution(ctx, input)
	if err != nil {
		s.logProvisioningError(ctx, logID, err.Error())
		return nil, fmt.Errorf("creating CloudFront distribution: %w", err)
	}

	dist := &CloudFrontDist{
		ID:               uuid.New().String(),
		OrgID:            orgID,
		DistributionType: "image_cdn",
		Domain:           domain,
		CloudFrontID:     *output.Distribution.Id,
		CloudFrontDomain: *output.Distribution.DomainName,
		OriginDomain:     s3Origin,
		ACMCertARN:       certARN,
		Status:           aws.ToString(output.Distribution.Status),
		Enabled:          true,
		PriceClass:       "PriceClass_100",
		CreatedAt:        time.Now(),
	}

	// Save to database
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mailing_cloudfront_distributions 
		(id, org_id, distribution_type, domain, cloudfront_id, cloudfront_domain, origin_domain, acm_cert_arn, status, enabled, price_class, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
	`, dist.ID, dist.OrgID, dist.DistributionType, dist.Domain, dist.CloudFrontID, dist.CloudFrontDomain,
		dist.OriginDomain, nullIfEmpty(dist.ACMCertARN), dist.Status, dist.Enabled, dist.PriceClass, dist.CreatedAt)
	if err != nil {
		s.logProvisioningError(ctx, logID, err.Error())
		return nil, fmt.Errorf("saving distribution to database: %w", err)
	}

	s.logProvisioningComplete(ctx, logID, dist.CloudFrontID, "")

	return dist, nil
}

// GetDistributionStatus gets the status of a CloudFront distribution
func (s *AWSInfrastructureService) GetDistributionStatus(ctx context.Context, distributionID string) (string, error) {
	output, err := s.cfClient.GetDistribution(ctx, &cloudfront.GetDistributionInput{
		Id: aws.String(distributionID),
	})
	if err != nil {
		return "", fmt.Errorf("getting distribution: %w", err)
	}

	status := aws.ToString(output.Distribution.Status)

	// Update database
	_, err = s.db.ExecContext(ctx, `
		UPDATE mailing_cloudfront_distributions 
		SET status = $1, updated_at = NOW()
		WHERE cloudfront_id = $2
	`, status, distributionID)
	if err != nil {
		log.Printf("Failed to update distribution status in DB: %v", err)
	}

	return status, nil
}

// ============================================
// S3 BUCKET MANAGEMENT
// ============================================

// CreateImageBucket creates an S3 bucket for image hosting
func (s *AWSInfrastructureService) CreateImageBucket(ctx context.Context, orgID, bucketName string) (*S3Bucket, error) {
	logID, err := s.logProvisioningStart(ctx, "s3_bucket", orgID, "create", map[string]string{
		"bucket_name": bucketName,
	})
	if err != nil {
		log.Printf("Failed to log provisioning start: %v", err)
	}

	// Create bucket
	createInput := &s3.CreateBucketInput{
		Bucket: aws.String(bucketName),
	}

	// Add location constraint for non-us-east-1 regions
	if s.region != "us-east-1" {
		createInput.CreateBucketConfiguration = &s3types.CreateBucketConfiguration{
			LocationConstraint: s3types.BucketLocationConstraint(s.region),
		}
	}

	_, err = s.s3Client.CreateBucket(ctx, createInput)
	if err != nil {
		// Check if bucket already exists
		if !strings.Contains(err.Error(), "BucketAlreadyOwnedByYou") {
			s.logProvisioningError(ctx, logID, err.Error())
			return nil, fmt.Errorf("creating S3 bucket: %w", err)
		}
	}

	// Allow public access for CloudFront (disable public access block)
	_, err = s.s3Client.PutPublicAccessBlock(ctx, &s3.PutPublicAccessBlockInput{
		Bucket: aws.String(bucketName),
		PublicAccessBlockConfiguration: &s3types.PublicAccessBlockConfiguration{
			BlockPublicAcls:       aws.Bool(false), // Allow ACLs for CloudFront OAI
			IgnorePublicAcls:      aws.Bool(false),
			BlockPublicPolicy:     aws.Bool(false), // Allow bucket policy for CloudFront
			RestrictPublicBuckets: aws.Bool(false),
		},
	})
	if err != nil {
		log.Printf("Warning: Failed to configure public access block: %v", err)
	}

	// Add bucket policy for CloudFront access
	policy := fmt.Sprintf(`{
		"Version": "2012-10-17",
		"Statement": [
			{
				"Sid": "PublicReadGetObject",
				"Effect": "Allow",
				"Principal": "*",
				"Action": "s3:GetObject",
				"Resource": "arn:aws:s3:::%s/*"
			}
		]
	}`, bucketName)

	_, err = s.s3Client.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{
		Bucket: aws.String(bucketName),
		Policy: aws.String(policy),
	})
	if err != nil {
		log.Printf("Warning: Failed to set bucket policy: %v", err)
	}

	// Enable CORS for browser uploads
	_, err = s.s3Client.PutBucketCors(ctx, &s3.PutBucketCorsInput{
		Bucket: aws.String(bucketName),
		CORSConfiguration: &s3types.CORSConfiguration{
			CORSRules: []s3types.CORSRule{
				{
					AllowedHeaders: []string{"*"},
					AllowedMethods: []string{"GET", "PUT", "POST"},
					AllowedOrigins: []string{"*"},
					MaxAgeSeconds:  aws.Int32(3600),
				},
			},
		},
	})
	if err != nil {
		log.Printf("Warning: Failed to set CORS: %v", err)
	}

	bucket := &S3Bucket{
		ID:                  uuid.New().String(),
		OrgID:               orgID,
		BucketName:          bucketName,
		BucketType:          "image_cdn",
		Region:              s.region,
		PublicAccessBlocked: false,
		VersioningEnabled:   false,
		CreatedAt:           time.Now(),
	}

	// Save to database
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mailing_s3_buckets (id, org_id, bucket_name, bucket_type, region, public_access_blocked, versioning_enabled, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (bucket_name) DO UPDATE SET updated_at = NOW()
	`, bucket.ID, bucket.OrgID, bucket.BucketName, bucket.BucketType, bucket.Region, bucket.PublicAccessBlocked, bucket.VersioningEnabled, bucket.CreatedAt)
	if err != nil {
		s.logProvisioningError(ctx, logID, err.Error())
		return nil, fmt.Errorf("saving bucket to database: %w", err)
	}

	s.logProvisioningComplete(ctx, logID, bucketName, fmt.Sprintf("arn:aws:s3:::%s", bucketName))

	return bucket, nil
}

// ============================================
// ROUTE53 DNS MANAGEMENT
// ============================================

// CreateDNSRecord creates a Route53 DNS record
func (s *AWSInfrastructureService) CreateDNSRecord(ctx context.Context, orgID, recordName, recordType, recordValue string, ttl int) (*Route53Record, error) {
	logID, err := s.logProvisioningStart(ctx, "route53", orgID, "create", map[string]string{
		"record_name": recordName,
		"record_type": recordType,
		"record_value": recordValue,
	})
	if err != nil {
		log.Printf("Failed to log provisioning start: %v", err)
	}

	// Ensure record name has trailing dot
	if !strings.HasSuffix(recordName, ".") {
		recordName = recordName + "."
	}

	change := r53types.Change{
		Action: r53types.ChangeActionUpsert,
		ResourceRecordSet: &r53types.ResourceRecordSet{
			Name: aws.String(recordName),
			Type: r53types.RRType(recordType),
			TTL:  aws.Int64(int64(ttl)),
			ResourceRecords: []r53types.ResourceRecord{
				{Value: aws.String(recordValue)},
			},
		},
	}

	// For ALIAS records to CloudFront
	if recordType == "A" && strings.Contains(recordValue, "cloudfront.net") {
		change.ResourceRecordSet.TTL = nil
		change.ResourceRecordSet.ResourceRecords = nil
		change.ResourceRecordSet.AliasTarget = &r53types.AliasTarget{
			HostedZoneId:         aws.String("Z2FDTNDATAQYW2"), // CloudFront hosted zone ID
			DNSName:              aws.String(recordValue),
			EvaluateTargetHealth: false,
		}
	}

	output, err := s.r53Client.ChangeResourceRecordSets(ctx, &route53.ChangeResourceRecordSetsInput{
		HostedZoneId: aws.String(s.hostedZoneID),
		ChangeBatch: &r53types.ChangeBatch{
			Changes: []r53types.Change{change},
			Comment: aws.String(fmt.Sprintf("Created by Ignite Mailing for org %s", orgID)),
		},
	})
	if err != nil {
		s.logProvisioningError(ctx, logID, err.Error())
		return nil, fmt.Errorf("creating Route53 record: %w", err)
	}

	record := &Route53Record{
		ID:           uuid.New().String(),
		OrgID:        orgID,
		HostedZoneID: s.hostedZoneID,
		RecordName:   recordName,
		RecordType:   recordType,
		RecordValue:  recordValue,
		TTL:          ttl,
		ChangeID:     *output.ChangeInfo.Id,
		Status:       string(output.ChangeInfo.Status),
		CreatedAt:    time.Now(),
	}

	// Save to database
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO mailing_route53_records (id, org_id, hosted_zone_id, record_name, record_type, record_value, ttl, change_id, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
	`, record.ID, record.OrgID, record.HostedZoneID, record.RecordName, record.RecordType, record.RecordValue, record.TTL, record.ChangeID, record.Status, record.CreatedAt)
	if err != nil {
		s.logProvisioningError(ctx, logID, err.Error())
		return nil, fmt.Errorf("saving record to database: %w", err)
	}

	s.logProvisioningComplete(ctx, logID, *output.ChangeInfo.Id, "")

	return record, nil
}

// CreateCloudFrontAlias creates an ALIAS record pointing to CloudFront
func (s *AWSInfrastructureService) CreateCloudFrontAlias(ctx context.Context, orgID, domain, cloudfrontDomain string) (*Route53Record, error) {
	return s.CreateDNSRecord(ctx, orgID, domain, "A", cloudfrontDomain, 0)
}

// ============================================
// PROVISIONING WORKFLOW
// ============================================

// ProvisionTrackingDomain provisions complete AWS infrastructure for a tracking domain
func (s *AWSInfrastructureService) ProvisionTrackingDomain(ctx context.Context, orgID, domain, originServer string) error {
	log.Printf("Starting provisioning for tracking domain: %s", domain)

	// Step 1: Request ACM certificate
	log.Printf("Step 1: Requesting ACM certificate for %s", domain)
	cert, err := s.RequestCertificate(ctx, orgID, domain)
	if err != nil {
		return fmt.Errorf("requesting certificate: %w", err)
	}

	// Step 2: Create validation records in Route53
	log.Printf("Step 2: Creating DNS validation records")
	err = s.CreateValidationRecords(ctx, cert.CertificateARN)
	if err != nil {
		return fmt.Errorf("creating validation records: %w", err)
	}

	// Step 3: Wait for certificate validation (async - could take a few minutes)
	log.Printf("Step 3: Waiting for certificate validation (this may take a few minutes)")
	go func() {
		ctx := context.Background()
		err := s.WaitForCertificateValidation(ctx, cert.CertificateARN, 10*time.Minute)
		if err != nil {
			log.Printf("Certificate validation failed: %v", err)
			return
		}

		log.Printf("Certificate validated, creating CloudFront distribution")

		// Step 4: Create CloudFront distribution
		dist, err := s.CreateTrackingDistribution(ctx, orgID, domain, originServer, cert.CertificateARN)
		if err != nil {
			log.Printf("Failed to create CloudFront distribution: %v", err)
			return
		}

		// Step 5: Create Route53 alias record
		log.Printf("Creating Route53 alias record")
		_, err = s.CreateCloudFrontAlias(ctx, orgID, domain, dist.CloudFrontDomain)
		if err != nil {
			log.Printf("Failed to create Route53 alias: %v", err)
			return
		}

		// Step 6: Update tracking domain in database with CloudFront details
		_, err = s.db.ExecContext(ctx, `
			UPDATE mailing_tracking_domains 
			SET ssl_status = 'active', 
			    cloudfront_id = $1, 
			    cloudfront_domain = $2, 
			    acm_cert_arn = $3,
			    ssl_provisioned = true,
			    updated_at = NOW()
			WHERE domain = $4
		`, dist.CloudFrontID, dist.CloudFrontDomain, cert.CertificateARN, domain)
		if err != nil {
			log.Printf("Failed to update tracking domain: %v", err)
			return
		}

		log.Printf("Successfully provisioned tracking domain: %s -> %s", domain, dist.CloudFrontDomain)
	}()

	return nil
}

// ProvisionImageCDN provisions complete AWS infrastructure for image CDN
func (s *AWSInfrastructureService) ProvisionImageCDN(ctx context.Context, orgID, domain, bucketName string) error {
	log.Printf("Starting provisioning for image CDN: %s with bucket %s", domain, bucketName)

	// Step 1: Create S3 bucket
	log.Printf("Step 1: Creating S3 bucket: %s", bucketName)
	_, err := s.CreateImageBucket(ctx, orgID, bucketName)
	if err != nil {
		return fmt.Errorf("creating S3 bucket: %w", err)
	}

	// Step 2: Request ACM certificate
	log.Printf("Step 2: Requesting ACM certificate for %s", domain)
	cert, err := s.RequestCertificate(ctx, orgID, domain)
	if err != nil {
		return fmt.Errorf("requesting certificate: %w", err)
	}

	// Step 3: Create validation records
	log.Printf("Step 3: Creating DNS validation records")
	err = s.CreateValidationRecords(ctx, cert.CertificateARN)
	if err != nil {
		return fmt.Errorf("creating validation records: %w", err)
	}

	// Continue async
	go func() {
		ctx := context.Background()

		// Step 4: Wait for certificate validation
		log.Printf("Step 4: Waiting for certificate validation")
		err := s.WaitForCertificateValidation(ctx, cert.CertificateARN, 10*time.Minute)
		if err != nil {
			log.Printf("Certificate validation failed: %v", err)
			return
		}

		// Step 5: Create CloudFront distribution
		log.Printf("Step 5: Creating CloudFront distribution")
		dist, err := s.CreateImageCDNDistribution(ctx, orgID, domain, bucketName, cert.CertificateARN)
		if err != nil {
			log.Printf("Failed to create CloudFront distribution: %v", err)
			return
		}

		// Step 6: Create Route53 alias record
		log.Printf("Step 6: Creating Route53 alias record")
		_, err = s.CreateCloudFrontAlias(ctx, orgID, domain, dist.CloudFrontDomain)
		if err != nil {
			log.Printf("Failed to create Route53 alias: %v", err)
			return
		}

		// Step 7: Update image domain in database
		_, err = s.db.ExecContext(ctx, `
			UPDATE mailing_image_domains 
			SET ssl_status = 'active', 
			    cloudfront_distribution_id = $1, 
			    cloudfront_domain = $2, 
			    acm_cert_arn = $3,
			    s3_bucket = $4,
			    verified = true,
			    updated_at = NOW()
			WHERE domain = $5
		`, dist.CloudFrontID, dist.CloudFrontDomain, cert.CertificateARN, bucketName, domain)
		if err != nil {
			log.Printf("Failed to update image domain: %v", err)
			return
		}

		log.Printf("Successfully provisioned image CDN: %s -> %s", domain, dist.CloudFrontDomain)
	}()

	return nil
}

// ============================================
// PROVISIONING LOG HELPERS
// ============================================

func (s *AWSInfrastructureService) logProvisioningStart(ctx context.Context, resourceType, resourceID, action string, params map[string]string) (string, error) {
	logID := uuid.New().String()
	paramsJSON, _ := json.Marshal(params)

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO mailing_aws_provisioning_log (id, resource_type, resource_id, action, status, request_params, started_at)
		VALUES ($1, $2, $3, $4, 'in_progress', $5, NOW())
	`, logID, resourceType, resourceID, action, paramsJSON)

	return logID, err
}

func (s *AWSInfrastructureService) logProvisioningComplete(ctx context.Context, logID, awsResourceID, awsResourceARN string) {
	_, err := s.db.ExecContext(ctx, `
		UPDATE mailing_aws_provisioning_log 
		SET status = 'completed', aws_resource_id = $1, aws_resource_arn = $2, completed_at = NOW()
		WHERE id = $3
	`, awsResourceID, awsResourceARN, logID)
	if err != nil {
		log.Printf("Failed to update provisioning log: %v", err)
	}
}

func (s *AWSInfrastructureService) logProvisioningError(ctx context.Context, logID, errorMsg string) {
	_, err := s.db.ExecContext(ctx, `
		UPDATE mailing_aws_provisioning_log 
		SET status = 'failed', error_message = $1, completed_at = NOW()
		WHERE id = $2
	`, errorMsg, logID)
	if err != nil {
		log.Printf("Failed to update provisioning log: %v", err)
	}
}

// ============================================
// QUERY METHODS
// ============================================

// GetCertificateByDomain retrieves a certificate by domain
func (s *AWSInfrastructureService) GetCertificateByDomain(ctx context.Context, domain string) (*ACMCertificate, error) {
	var cert ACMCertificate
	var validationJSON []byte
	var issuedAt, expiresAt sql.NullTime

	err := s.db.QueryRowContext(ctx, `
		SELECT id, org_id, domain, certificate_arn, status, validation_method, validation_records, issued_at, expires_at, created_at
		FROM mailing_acm_certificates
		WHERE domain = $1
		ORDER BY created_at DESC
		LIMIT 1
	`, domain).Scan(
		&cert.ID, &cert.OrgID, &cert.Domain, &cert.CertificateARN, &cert.Status,
		&cert.ValidationMethod, &validationJSON, &issuedAt, &expiresAt, &cert.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying certificate: %w", err)
	}

	if validationJSON != nil {
		json.Unmarshal(validationJSON, &cert.ValidationRecords)
	}
	if issuedAt.Valid {
		cert.IssuedAt = &issuedAt.Time
	}
	if expiresAt.Valid {
		cert.ExpiresAt = &expiresAt.Time
	}

	return &cert, nil
}

// GetDistributionByDomain retrieves a CloudFront distribution by domain
func (s *AWSInfrastructureService) GetDistributionByDomain(ctx context.Context, domain string) (*CloudFrontDist, error) {
	var dist CloudFrontDist
	var certARN sql.NullString

	err := s.db.QueryRowContext(ctx, `
		SELECT id, org_id, distribution_type, domain, cloudfront_id, cloudfront_domain, origin_domain, acm_cert_arn, status, enabled, price_class, created_at
		FROM mailing_cloudfront_distributions
		WHERE domain = $1
	`, domain).Scan(
		&dist.ID, &dist.OrgID, &dist.DistributionType, &dist.Domain, &dist.CloudFrontID,
		&dist.CloudFrontDomain, &dist.OriginDomain, &certARN, &dist.Status, &dist.Enabled, &dist.PriceClass, &dist.CreatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("querying distribution: %w", err)
	}

	if certARN.Valid {
		dist.ACMCertARN = certARN.String
	}

	return &dist, nil
}

// ListProvisioningLogs lists provisioning logs for a resource
func (s *AWSInfrastructureService) ListProvisioningLogs(ctx context.Context, resourceType, resourceID string, limit int) ([]ProvisioningLog, error) {
	if limit <= 0 {
		limit = 20
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, resource_type, resource_id, action, aws_resource_id, aws_resource_arn, status, request_params, response_data, error_message, started_at, completed_at
		FROM mailing_aws_provisioning_log
		WHERE resource_type = $1 AND resource_id = $2
		ORDER BY started_at DESC
		LIMIT $3
	`, resourceType, resourceID, limit)
	if err != nil {
		return nil, fmt.Errorf("querying provisioning logs: %w", err)
	}
	defer rows.Close()

	var logs []ProvisioningLog
	for rows.Next() {
		var log ProvisioningLog
		var awsResourceID, awsResourceARN, errorMsg sql.NullString
		var requestParams, responseData []byte
		var completedAt sql.NullTime

		if err := rows.Scan(
			&log.ID, &log.ResourceType, &log.ResourceID, &log.Action,
			&awsResourceID, &awsResourceARN, &log.Status, &requestParams, &responseData,
			&errorMsg, &log.StartedAt, &completedAt,
		); err != nil {
			continue
		}

		log.AWSResourceID = awsResourceID.String
		log.AWSResourceARN = awsResourceARN.String
		log.ErrorMessage = errorMsg.String
		log.RequestParams = requestParams
		log.ResponseData = responseData
		if completedAt.Valid {
			log.CompletedAt = &completedAt.Time
		}

		logs = append(logs, log)
	}

	if logs == nil {
		logs = []ProvisioningLog{}
	}

	return logs, nil
}
