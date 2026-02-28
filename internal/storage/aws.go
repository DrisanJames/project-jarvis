package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/ignite/sparkpost-monitor/internal/mailgun"
	"github.com/ignite/sparkpost-monitor/internal/ses"
	"github.com/ignite/sparkpost-monitor/internal/sparkpost"
)

// AWSStorage provides AWS-backed storage using DynamoDB and S3
type AWSStorage struct {
	dynamoDB  *dynamodb.Client
	s3Client  *s3.Client
	tableName string
	bucket    string
	region    string
}

// DynamoDBItem represents an item stored in DynamoDB
type DynamoDBItem struct {
	PK        string `dynamodbav:"PK"`
	SK        string `dynamodbav:"SK"`
	Data      string `dynamodbav:"Data"`
	Timestamp string `dynamodbav:"Timestamp"`
	TTL       int64  `dynamodbav:"TTL,omitempty"`
}

// NewAWSStorage creates a new AWS storage instance
func NewAWSStorage(ctx context.Context, tableName, bucket, region, profile string) (*AWSStorage, error) {
	var cfg aws.Config
	var err error

	if profile != "" {
		cfg, err = config.LoadDefaultConfig(ctx,
			config.WithRegion(region),
			config.WithSharedConfigProfile(profile),
		)
	} else {
		cfg, err = config.LoadDefaultConfig(ctx,
			config.WithRegion(region),
		)
	}
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	return &AWSStorage{
		dynamoDB:  dynamodb.NewFromConfig(cfg),
		s3Client:  s3.NewFromConfig(cfg),
		tableName: tableName,
		bucket:    bucket,
		region:    region,
	}, nil
}

// SaveMetricsToDynamoDB saves metrics to DynamoDB
func (s *AWSStorage) SaveMetricsToDynamoDB(ctx context.Context, entityType, entityName string, metrics interface{}) error {
	data, err := json.Marshal(metrics)
	if err != nil {
		return fmt.Errorf("marshaling metrics: %w", err)
	}

	item := DynamoDBItem{
		PK:        fmt.Sprintf("%s#%s", entityType, entityName),
		SK:        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		Data:      string(data),
		Timestamp: time.Now().UTC().Format(time.RFC3339),
		TTL:       time.Now().Add(90 * 24 * time.Hour).Unix(), // 90 day TTL
	}

	av, err := attributevalue.MarshalMap(item)
	if err != nil {
		return fmt.Errorf("marshaling item: %w", err)
	}

	_, err = s.dynamoDB.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.tableName),
		Item:      av,
	})
	if err != nil {
		return fmt.Errorf("putting item to DynamoDB: %w", err)
	}

	return nil
}

// SaveISPMetrics saves ISP metrics to DynamoDB
func (s *AWSStorage) SaveISPMetrics(ctx context.Context, metrics []sparkpost.ISPMetrics) error {
	for _, isp := range metrics {
		if err := s.SaveMetricsToDynamoDB(ctx, "ISP", isp.Provider, isp); err != nil {
			return err
		}
	}
	return nil
}

// SaveIPMetrics saves IP metrics to DynamoDB
func (s *AWSStorage) SaveIPMetrics(ctx context.Context, metrics []sparkpost.IPMetrics) error {
	for _, ip := range metrics {
		if err := s.SaveMetricsToDynamoDB(ctx, "IP", ip.IP, ip); err != nil {
			return err
		}
	}
	return nil
}

// SaveDomainMetrics saves domain metrics to DynamoDB
func (s *AWSStorage) SaveDomainMetrics(ctx context.Context, metrics []sparkpost.DomainMetrics) error {
	for _, domain := range metrics {
		if err := s.SaveMetricsToDynamoDB(ctx, "DOMAIN", domain.Domain, domain); err != nil {
			return err
		}
	}
	return nil
}

// GetISPMetrics retrieves ISP metrics from DynamoDB
func (s *AWSStorage) GetISPMetrics(ctx context.Context, provider string, from, to time.Time) ([]sparkpost.ISPMetrics, error) {
	result, err := s.dynamoDB.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		KeyConditionExpression: aws.String("PK = :pk AND SK BETWEEN :from AND :to"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":   &types.AttributeValueMemberS{Value: fmt.Sprintf("ISP#%s", provider)},
			":from": &types.AttributeValueMemberS{Value: from.UTC().Format("2006-01-02T15:04:05Z")},
			":to":   &types.AttributeValueMemberS{Value: to.UTC().Format("2006-01-02T15:04:05Z")},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("querying DynamoDB: %w", err)
	}

	var metrics []sparkpost.ISPMetrics
	for _, item := range result.Items {
		var dbItem DynamoDBItem
		if err := attributevalue.UnmarshalMap(item, &dbItem); err != nil {
			continue
		}
		var isp sparkpost.ISPMetrics
		if err := json.Unmarshal([]byte(dbItem.Data), &isp); err != nil {
			continue
		}
		metrics = append(metrics, isp)
	}

	return metrics, nil
}

// SaveToS3 saves data to S3
func (s *AWSStorage) SaveToS3(ctx context.Context, key string, data interface{}) error {
	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling data: %w", err)
	}

	_, err = s.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(jsonData),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("putting object to S3: %w", err)
	}

	return nil
}

// SaveMetricsToS3 saves processed metrics to S3
func (s *AWSStorage) SaveMetricsToS3(ctx context.Context, metrics []sparkpost.ProcessedMetrics) error {
	key := fmt.Sprintf("metrics/%s/summary.json", time.Now().UTC().Format("2006/01/02"))
	return s.SaveToS3(ctx, key, metrics)
}

// SaveSignalsToS3 saves signals data to S3
func (s *AWSStorage) SaveSignalsToS3(ctx context.Context, signals sparkpost.SignalsData) error {
	key := fmt.Sprintf("signals/%s/%s.json", 
		time.Now().UTC().Format("2006/01/02"),
		time.Now().UTC().Format("15-04-05"))
	return s.SaveToS3(ctx, key, signals)
}

// SaveBaselineToS3 saves baselines to S3
func (s *AWSStorage) SaveBaselineToS3(ctx context.Context, baseline *Baseline) error {
	key := fmt.Sprintf("baselines/%s/%s.json", baseline.EntityType, baseline.EntityName)
	return s.SaveToS3(ctx, key, baseline)
}

// SaveCorrelationsToS3 saves correlations to S3
func (s *AWSStorage) SaveCorrelationsToS3(ctx context.Context, correlations []Correlation) error {
	key := "learned/correlations.json"
	return s.SaveToS3(ctx, key, correlations)
}

// GetFromS3 retrieves data from S3
func (s *AWSStorage) GetFromS3(ctx context.Context, key string, target interface{}) error {
	result, err := s.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("getting object from S3: %w", err)
	}
	defer result.Body.Close()

	data, err := io.ReadAll(result.Body)
	if err != nil {
		return fmt.Errorf("reading S3 object body: %w", err)
	}

	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("unmarshaling S3 data: %w", err)
	}

	return nil
}

// GetBaselineFromS3 retrieves a baseline from S3
func (s *AWSStorage) GetBaselineFromS3(ctx context.Context, entityType, entityName string) (*Baseline, error) {
	key := fmt.Sprintf("baselines/%s/%s.json", entityType, entityName)
	var baseline Baseline
	if err := s.GetFromS3(ctx, key, &baseline); err != nil {
		return nil, err
	}
	return &baseline, nil
}

// GetCorrelationsFromS3 retrieves correlations from S3
func (s *AWSStorage) GetCorrelationsFromS3(ctx context.Context) ([]Correlation, error) {
	var correlations []Correlation
	if err := s.GetFromS3(ctx, "learned/correlations.json", &correlations); err != nil {
		return nil, err
	}
	return correlations, nil
}

// SaveToS3Bucket saves data to a specific S3 bucket
func (s *AWSStorage) SaveToS3Bucket(ctx context.Context, bucket, key string, data interface{}) error {
	jsonData, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling data: %w", err)
	}

	targetBucket := bucket
	if targetBucket == "" {
		targetBucket = s.bucket
	}

	_, err = s.s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(targetBucket),
		Key:         aws.String(key),
		Body:        bytes.NewReader(jsonData),
		ContentType: aws.String("application/json"),
	})
	if err != nil {
		return fmt.Errorf("putting object to S3 bucket %s: %w", targetBucket, err)
	}

	return nil
}

// GetFromS3Bucket retrieves data from a specific S3 bucket
func (s *AWSStorage) GetFromS3Bucket(ctx context.Context, bucket, key string, target interface{}) error {
	targetBucket := bucket
	if targetBucket == "" {
		targetBucket = s.bucket
	}

	result, err := s.s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(targetBucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("getting object from S3 bucket %s: %w", targetBucket, err)
	}
	defer result.Body.Close()

	data, err := io.ReadAll(result.Body)
	if err != nil {
		return fmt.Errorf("reading S3 object body: %w", err)
	}

	if err := json.Unmarshal(data, target); err != nil {
		return fmt.Errorf("unmarshaling S3 data: %w", err)
	}

	return nil
}

// ListBaselinesFromS3 lists all baselines from S3
func (s *AWSStorage) ListBaselinesFromS3(ctx context.Context) (map[string]*Baseline, error) {
	baselines := make(map[string]*Baseline)

	// List all baseline files
	paginator := s3.NewListObjectsV2Paginator(s.s3Client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String("baselines/"),
	})

	for paginator.HasMorePages() {
		page, err := paginator.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing S3 objects: %w", err)
		}

		for _, obj := range page.Contents {
			if !strings.HasSuffix(*obj.Key, ".json") {
				continue
			}

			var baseline Baseline
			if err := s.GetFromS3(ctx, *obj.Key, &baseline); err != nil {
				continue
			}

			key := fmt.Sprintf("%s:%s", baseline.EntityType, baseline.EntityName)
			baselines[key] = &baseline
		}
	}

	return baselines, nil
}

// SaveTimeSeriesMetrics saves time series data to S3 organized by date
func (s *AWSStorage) SaveTimeSeriesMetrics(ctx context.Context, series []sparkpost.TimeSeries) error {
	key := fmt.Sprintf("timeseries/%s/data.json", time.Now().UTC().Format("2006/01/02"))
	return s.SaveToS3(ctx, key, series)
}

// Mailgun-specific AWS storage methods

// SaveMailgunMetricsToS3 saves Mailgun processed metrics to S3
func (s *AWSStorage) SaveMailgunMetricsToS3(ctx context.Context, metrics []mailgun.ProcessedMetrics) error {
	key := fmt.Sprintf("mailgun/metrics/%s/summary.json", time.Now().UTC().Format("2006/01/02"))
	return s.SaveToS3(ctx, key, metrics)
}

// SaveMailgunISPMetrics saves Mailgun ISP metrics to DynamoDB
func (s *AWSStorage) SaveMailgunISPMetrics(ctx context.Context, metrics []mailgun.ISPMetrics) error {
	for _, isp := range metrics {
		if err := s.SaveMetricsToDynamoDB(ctx, "MAILGUN_ISP", isp.Provider, isp); err != nil {
			return err
		}
	}
	return nil
}

// SaveMailgunDomainMetrics saves Mailgun domain metrics to DynamoDB
func (s *AWSStorage) SaveMailgunDomainMetrics(ctx context.Context, metrics []mailgun.DomainMetrics) error {
	for _, domain := range metrics {
		if err := s.SaveMetricsToDynamoDB(ctx, "MAILGUN_DOMAIN", domain.Domain, domain); err != nil {
			return err
		}
	}
	return nil
}

// SaveMailgunSignalsToS3 saves Mailgun signals data to S3
func (s *AWSStorage) SaveMailgunSignalsToS3(ctx context.Context, signals mailgun.SignalsData) error {
	key := fmt.Sprintf("mailgun/signals/%s/%s.json",
		time.Now().UTC().Format("2006/01/02"),
		time.Now().UTC().Format("15-04-05"))
	return s.SaveToS3(ctx, key, signals)
}

// GetMailgunISPMetrics retrieves Mailgun ISP metrics from DynamoDB
func (s *AWSStorage) GetMailgunISPMetrics(ctx context.Context, provider string, from, to time.Time) ([]mailgun.ISPMetrics, error) {
	result, err := s.dynamoDB.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		KeyConditionExpression: aws.String("PK = :pk AND SK BETWEEN :from AND :to"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":   &types.AttributeValueMemberS{Value: fmt.Sprintf("MAILGUN_ISP#%s", provider)},
			":from": &types.AttributeValueMemberS{Value: from.UTC().Format("2006-01-02T15:04:05Z")},
			":to":   &types.AttributeValueMemberS{Value: to.UTC().Format("2006-01-02T15:04:05Z")},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("querying DynamoDB: %w", err)
	}

	var metrics []mailgun.ISPMetrics
	for _, item := range result.Items {
		var dbItem DynamoDBItem
		if err := attributevalue.UnmarshalMap(item, &dbItem); err != nil {
			continue
		}
		var isp mailgun.ISPMetrics
		if err := json.Unmarshal([]byte(dbItem.Data), &isp); err != nil {
			continue
		}
		metrics = append(metrics, isp)
	}

	return metrics, nil
}

// SES-specific AWS storage methods

// SaveSESMetricsToS3 saves SES processed metrics to S3
func (s *AWSStorage) SaveSESMetricsToS3(ctx context.Context, metrics []ses.ProcessedMetrics) error {
	key := fmt.Sprintf("ses/metrics/%s/summary.json", time.Now().UTC().Format("2006/01/02"))
	return s.SaveToS3(ctx, key, metrics)
}

// SaveSESISPMetrics saves SES ISP metrics to DynamoDB
func (s *AWSStorage) SaveSESISPMetrics(ctx context.Context, metrics []ses.ISPMetrics) error {
	for _, isp := range metrics {
		if err := s.SaveMetricsToDynamoDB(ctx, "SES_ISP", isp.Provider, isp); err != nil {
			return err
		}
	}
	return nil
}

// SaveSESSignalsToS3 saves SES signals data to S3
func (s *AWSStorage) SaveSESSignalsToS3(ctx context.Context, signals ses.SignalsData) error {
	key := fmt.Sprintf("ses/signals/%s/%s.json",
		time.Now().UTC().Format("2006/01/02"),
		time.Now().UTC().Format("15-04-05"))
	return s.SaveToS3(ctx, key, signals)
}

// GetSESISPMetrics retrieves SES ISP metrics from DynamoDB
func (s *AWSStorage) GetSESISPMetrics(ctx context.Context, provider string, from, to time.Time) ([]ses.ISPMetrics, error) {
	result, err := s.dynamoDB.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		KeyConditionExpression: aws.String("PK = :pk AND SK BETWEEN :from AND :to"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":   &types.AttributeValueMemberS{Value: fmt.Sprintf("SES_ISP#%s", provider)},
			":from": &types.AttributeValueMemberS{Value: from.UTC().Format("2006-01-02T15:04:05Z")},
			":to":   &types.AttributeValueMemberS{Value: to.UTC().Format("2006-01-02T15:04:05Z")},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("querying DynamoDB: %w", err)
	}

	var metrics []ses.ISPMetrics
	for _, item := range result.Items {
		var dbItem DynamoDBItem
		if err := attributevalue.UnmarshalMap(item, &dbItem); err != nil {
			continue
		}
		var isp ses.ISPMetrics
		if err := json.Unmarshal([]byte(dbItem.Data), &isp); err != nil {
			continue
		}
		metrics = append(metrics, isp)
	}

	return metrics, nil
}

// CostConfiguration represents a persisted cost configuration
type CostConfiguration struct {
	PK          string           `dynamodbav:"PK"`
	SK          string           `dynamodbav:"SK"`
	CostItems   []CostConfigItem `dynamodbav:"CostItems"`
	LastUpdated string           `dynamodbav:"LastUpdated"`
	UpdatedBy   string           `dynamodbav:"UpdatedBy,omitempty"`
}

// CostConfigItem represents a single cost item configuration
type CostConfigItem struct {
	Name         string  `json:"name" dynamodbav:"Name"`
	Category     string  `json:"category" dynamodbav:"Category"`
	Type         string  `json:"type" dynamodbav:"Type"` // "vendor", "esp", "payroll", "revenue_share"
	MonthlyCost  float64 `json:"monthly_cost" dynamodbav:"MonthlyCost"`
	OriginalCost float64 `json:"original_cost" dynamodbav:"OriginalCost"`
	IsOverridden bool    `json:"is_overridden" dynamodbav:"IsOverridden"`
}

// SaveCostConfiguration saves cost configuration to DynamoDB
func (s *AWSStorage) SaveCostConfiguration(ctx context.Context, configType string, items []CostConfigItem) error {
	config := CostConfiguration{
		PK:          "COST_CONFIG",
		SK:          configType,
		CostItems:   items,
		LastUpdated: time.Now().UTC().Format(time.RFC3339),
	}

	av, err := attributevalue.MarshalMap(config)
	if err != nil {
		return fmt.Errorf("marshaling cost config: %w", err)
	}

	_, err = s.dynamoDB.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.tableName),
		Item:      av,
	})
	if err != nil {
		return fmt.Errorf("putting cost config to DynamoDB: %w", err)
	}

	return nil
}

// GetCostConfiguration retrieves cost configuration from DynamoDB
func (s *AWSStorage) GetCostConfiguration(ctx context.Context, configType string) (*CostConfiguration, error) {
	result, err := s.dynamoDB.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "COST_CONFIG"},
			"SK": &types.AttributeValueMemberS{Value: configType},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("getting cost config from DynamoDB: %w", err)
	}

	if result.Item == nil {
		return nil, nil // No config found
	}

	var config CostConfiguration
	if err := attributevalue.UnmarshalMap(result.Item, &config); err != nil {
		return nil, fmt.Errorf("unmarshaling cost config: %w", err)
	}

	return &config, nil
}

// GetAllCostConfigurations retrieves all cost configurations from DynamoDB
func (s *AWSStorage) GetAllCostConfigurations(ctx context.Context) (map[string][]CostConfigItem, error) {
	result, err := s.dynamoDB.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		KeyConditionExpression: aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: "COST_CONFIG"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("querying cost configs from DynamoDB: %w", err)
	}

	configs := make(map[string][]CostConfigItem)
	for _, item := range result.Items {
		var config CostConfiguration
		if err := attributevalue.UnmarshalMap(item, &config); err != nil {
			continue
		}
		configs[config.SK] = config.CostItems
	}

	return configs, nil
}

// DeleteCostConfiguration removes a cost configuration from DynamoDB
func (s *AWSStorage) DeleteCostConfiguration(ctx context.Context, configType string) error {
	_, err := s.dynamoDB.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "COST_CONFIG"},
			"SK": &types.AttributeValueMemberS{Value: configType},
		},
	})
	if err != nil {
		return fmt.Errorf("deleting cost config from DynamoDB: %w", err)
	}
	return nil
}

// RoutingPlan represents a saved routing plan configuration
type RoutingPlan struct {
	PK          string        `dynamodbav:"PK"`
	SK          string        `dynamodbav:"SK"`
	ID          string        `dynamodbav:"ID" json:"id"`
	Name        string        `dynamodbav:"Name" json:"name"`
	Description string        `dynamodbav:"Description,omitempty" json:"description,omitempty"`
	Routes      []RoutingRule `dynamodbav:"Routes" json:"routes"`
	CreatedAt   string        `dynamodbav:"CreatedAt" json:"created_at"`
	UpdatedAt   string        `dynamodbav:"UpdatedAt" json:"updated_at"`
	IsActive    bool          `dynamodbav:"IsActive" json:"is_active"`
}

// RoutingRule represents a single ISP-to-ESP routing rule
type RoutingRule struct {
	ISPID   string `dynamodbav:"ISPID" json:"isp_id"`
	ISPName string `dynamodbav:"ISPName" json:"isp_name"`
	ESPID   string `dynamodbav:"ESPID" json:"esp_id"`
	ESPName string `dynamodbav:"ESPName" json:"esp_name"`
}

// SaveRoutingPlan saves a routing plan to DynamoDB
func (s *AWSStorage) SaveRoutingPlan(ctx context.Context, plan *RoutingPlan) error {
	now := time.Now().UTC().Format(time.RFC3339)
	
	// Generate ID if not present
	if plan.ID == "" {
		plan.ID = fmt.Sprintf("plan_%d", time.Now().UnixNano())
	}
	
	plan.PK = "ROUTING_PLAN"
	plan.SK = plan.ID
	plan.UpdatedAt = now
	if plan.CreatedAt == "" {
		plan.CreatedAt = now
	}

	av, err := attributevalue.MarshalMap(plan)
	if err != nil {
		return fmt.Errorf("marshaling routing plan: %w", err)
	}

	_, err = s.dynamoDB.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.tableName),
		Item:      av,
	})
	if err != nil {
		return fmt.Errorf("saving routing plan to DynamoDB: %w", err)
	}

	return nil
}

// GetRoutingPlan retrieves a specific routing plan from DynamoDB
func (s *AWSStorage) GetRoutingPlan(ctx context.Context, planID string) (*RoutingPlan, error) {
	result, err := s.dynamoDB.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "ROUTING_PLAN"},
			"SK": &types.AttributeValueMemberS{Value: planID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("getting routing plan from DynamoDB: %w", err)
	}

	if result.Item == nil {
		return nil, nil
	}

	var plan RoutingPlan
	if err := attributevalue.UnmarshalMap(result.Item, &plan); err != nil {
		return nil, fmt.Errorf("unmarshaling routing plan: %w", err)
	}

	return &plan, nil
}

// GetAllRoutingPlans retrieves all routing plans from DynamoDB
func (s *AWSStorage) GetAllRoutingPlans(ctx context.Context) ([]RoutingPlan, error) {
	result, err := s.dynamoDB.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		KeyConditionExpression: aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: "ROUTING_PLAN"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("querying routing plans from DynamoDB: %w", err)
	}

	var plans []RoutingPlan
	for _, item := range result.Items {
		var plan RoutingPlan
		if err := attributevalue.UnmarshalMap(item, &plan); err != nil {
			continue
		}
		plans = append(plans, plan)
	}

	return plans, nil
}

// DeleteRoutingPlan removes a routing plan from DynamoDB
func (s *AWSStorage) DeleteRoutingPlan(ctx context.Context, planID string) error {
	_, err := s.dynamoDB.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "ROUTING_PLAN"},
			"SK": &types.AttributeValueMemberS{Value: planID},
		},
	})
	if err != nil {
		return fmt.Errorf("deleting routing plan from DynamoDB: %w", err)
	}
	return nil
}

// SetActiveRoutingPlan sets a specific plan as active and deactivates others
func (s *AWSStorage) SetActiveRoutingPlan(ctx context.Context, planID string) error {
	// First, get all plans
	plans, err := s.GetAllRoutingPlans(ctx)
	if err != nil {
		return err
	}

	// Update each plan
	for _, plan := range plans {
		plan.IsActive = (plan.ID == planID)
		if err := s.SaveRoutingPlan(ctx, &plan); err != nil {
			return fmt.Errorf("updating plan %s active status: %w", plan.ID, err)
		}
	}

	return nil
}

// GetActiveRoutingPlan retrieves the currently active routing plan
func (s *AWSStorage) GetActiveRoutingPlan(ctx context.Context) (*RoutingPlan, error) {
	plans, err := s.GetAllRoutingPlans(ctx)
	if err != nil {
		return nil, err
	}

	for _, plan := range plans {
		if plan.IsActive {
			return &plan, nil
		}
	}

	return nil, nil
}

// SuggestionStatus represents the status of a suggestion
type SuggestionStatus string

const (
	SuggestionStatusPending  SuggestionStatus = "pending"
	SuggestionStatusResolved SuggestionStatus = "resolved"
	SuggestionStatusDenied   SuggestionStatus = "denied"
)

// Suggestion represents a user improvement suggestion
type Suggestion struct {
	PK                 string           `dynamodbav:"PK"`
	SK                 string           `dynamodbav:"SK"`
	ID                 string           `dynamodbav:"ID" json:"id"`
	SubmittedByEmail   string           `dynamodbav:"SubmittedByEmail" json:"submitted_by_email"`
	SubmittedByName    string           `dynamodbav:"SubmittedByName" json:"submitted_by_name"`
	Area               string           `dynamodbav:"Area" json:"area"`
	AreaContext        string           `dynamodbav:"AreaContext,omitempty" json:"area_context,omitempty"`
	OriginalSuggestion string           `dynamodbav:"OriginalSuggestion" json:"original_suggestion"`
	Requirements       string           `dynamodbav:"Requirements,omitempty" json:"requirements,omitempty"`
	Status             SuggestionStatus `dynamodbav:"Status" json:"status"`
	ResolutionNotes    string           `dynamodbav:"ResolutionNotes,omitempty" json:"resolution_notes,omitempty"`
	CreatedAt          string           `dynamodbav:"CreatedAt" json:"created_at"`
	UpdatedAt          string           `dynamodbav:"UpdatedAt" json:"updated_at"`
}

// SaveSuggestion saves a suggestion to DynamoDB
func (s *AWSStorage) SaveSuggestion(ctx context.Context, suggestion *Suggestion) error {
	now := time.Now().UTC().Format(time.RFC3339)

	// Generate ID if not present
	if suggestion.ID == "" {
		suggestion.ID = fmt.Sprintf("sug_%d", time.Now().UnixNano())
	}

	suggestion.PK = "SUGGESTION"
	suggestion.SK = suggestion.ID
	suggestion.UpdatedAt = now
	if suggestion.CreatedAt == "" {
		suggestion.CreatedAt = now
	}
	if suggestion.Status == "" {
		suggestion.Status = SuggestionStatusPending
	}

	av, err := attributevalue.MarshalMap(suggestion)
	if err != nil {
		return fmt.Errorf("marshaling suggestion: %w", err)
	}

	_, err = s.dynamoDB.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(s.tableName),
		Item:      av,
	})
	if err != nil {
		return fmt.Errorf("saving suggestion to DynamoDB: %w", err)
	}

	return nil
}

// GetSuggestion retrieves a specific suggestion from DynamoDB
func (s *AWSStorage) GetSuggestion(ctx context.Context, suggestionID string) (*Suggestion, error) {
	result, err := s.dynamoDB.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "SUGGESTION"},
			"SK": &types.AttributeValueMemberS{Value: suggestionID},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("getting suggestion from DynamoDB: %w", err)
	}

	if result.Item == nil {
		return nil, nil
	}

	var suggestion Suggestion
	if err := attributevalue.UnmarshalMap(result.Item, &suggestion); err != nil {
		return nil, fmt.Errorf("unmarshaling suggestion: %w", err)
	}

	return &suggestion, nil
}

// GetAllSuggestions retrieves all suggestions from DynamoDB
func (s *AWSStorage) GetAllSuggestions(ctx context.Context) ([]Suggestion, error) {
	result, err := s.dynamoDB.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		KeyConditionExpression: aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: "SUGGESTION"},
		},
		ScanIndexForward: aws.Bool(false), // Most recent first
	})
	if err != nil {
		return nil, fmt.Errorf("querying suggestions from DynamoDB: %w", err)
	}

	var suggestions []Suggestion
	for _, item := range result.Items {
		var suggestion Suggestion
		if err := attributevalue.UnmarshalMap(item, &suggestion); err != nil {
			continue
		}
		suggestions = append(suggestions, suggestion)
	}

	return suggestions, nil
}

// GetSuggestionsByStatus retrieves suggestions filtered by status
func (s *AWSStorage) GetSuggestionsByStatus(ctx context.Context, status SuggestionStatus) ([]Suggestion, error) {
	result, err := s.dynamoDB.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(s.tableName),
		KeyConditionExpression: aws.String("PK = :pk"),
		FilterExpression:       aws.String("#status = :status"),
		ExpressionAttributeNames: map[string]string{
			"#status": "Status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk":     &types.AttributeValueMemberS{Value: "SUGGESTION"},
			":status": &types.AttributeValueMemberS{Value: string(status)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("querying suggestions by status from DynamoDB: %w", err)
	}

	var suggestions []Suggestion
	for _, item := range result.Items {
		var suggestion Suggestion
		if err := attributevalue.UnmarshalMap(item, &suggestion); err != nil {
			continue
		}
		suggestions = append(suggestions, suggestion)
	}

	return suggestions, nil
}

// UpdateSuggestionStatus updates the status of a suggestion
func (s *AWSStorage) UpdateSuggestionStatus(ctx context.Context, suggestionID string, status SuggestionStatus, resolutionNotes string) error {
	now := time.Now().UTC().Format(time.RFC3339)

	_, err := s.dynamoDB.UpdateItem(ctx, &dynamodb.UpdateItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "SUGGESTION"},
			"SK": &types.AttributeValueMemberS{Value: suggestionID},
		},
		UpdateExpression: aws.String("SET #status = :status, ResolutionNotes = :notes, UpdatedAt = :updated"),
		ExpressionAttributeNames: map[string]string{
			"#status": "Status",
		},
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":status":  &types.AttributeValueMemberS{Value: string(status)},
			":notes":   &types.AttributeValueMemberS{Value: resolutionNotes},
			":updated": &types.AttributeValueMemberS{Value: now},
		},
	})
	if err != nil {
		return fmt.Errorf("updating suggestion status in DynamoDB: %w", err)
	}

	return nil
}

// DeleteSuggestion removes a suggestion from DynamoDB
func (s *AWSStorage) DeleteSuggestion(ctx context.Context, suggestionID string) error {
	_, err := s.dynamoDB.DeleteItem(ctx, &dynamodb.DeleteItemInput{
		TableName: aws.String(s.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "SUGGESTION"},
			"SK": &types.AttributeValueMemberS{Value: suggestionID},
		},
	})
	if err != nil {
		return fmt.Errorf("deleting suggestion from DynamoDB: %w", err)
	}
	return nil
}
