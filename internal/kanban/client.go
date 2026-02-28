package kanban

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/feature/dynamodb/attributevalue"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb"
	"github.com/aws/aws-sdk-go-v2/service/dynamodb/types"
)

// Client provides DynamoDB operations for Kanban data
type Client struct {
	dynamoDB  *dynamodb.Client
	tableName string
}

// NewClient creates a new Kanban DynamoDB client
func NewClient(ctx context.Context, tableName, region, profile string) (*Client, error) {
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

	return &Client{
		dynamoDB:  dynamodb.NewFromConfig(cfg),
		tableName: tableName,
	}, nil
}

// GetBoard retrieves the Kanban board from DynamoDB
func (c *Client) GetBoard(ctx context.Context) (*KanbanBoard, error) {
	result, err := c.dynamoDB.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(c.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "KANBAN#default"},
			"SK": &types.AttributeValueMemberS{Value: "BOARD"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("getting board from DynamoDB: %w", err)
	}

	// If board doesn't exist, create default
	if result.Item == nil {
		board := NewDefaultBoard()
		if err := c.SaveBoard(ctx, board); err != nil {
			return nil, fmt.Errorf("creating default board: %w", err)
		}
		return board, nil
	}

	var board KanbanBoard
	if err := attributevalue.UnmarshalMap(result.Item, &board); err != nil {
		return nil, fmt.Errorf("unmarshaling board: %w", err)
	}

	return &board, nil
}

// SaveBoard saves the Kanban board to DynamoDB
func (c *Client) SaveBoard(ctx context.Context, board *KanbanBoard) error {
	board.LastModified = time.Now()

	av, err := attributevalue.MarshalMap(board)
	if err != nil {
		return fmt.Errorf("marshaling board: %w", err)
	}

	_, err = c.dynamoDB.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(c.tableName),
		Item:      av,
	})
	if err != nil {
		return fmt.Errorf("putting board to DynamoDB: %w", err)
	}

	return nil
}

// GetActiveIssues retrieves the active issues fingerprint map
func (c *Client) GetActiveIssues(ctx context.Context) (*ActiveIssues, error) {
	result, err := c.dynamoDB.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(c.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "KANBAN#issues"},
			"SK": &types.AttributeValueMemberS{Value: "ACTIVE"},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("getting active issues from DynamoDB: %w", err)
	}

	// If doesn't exist, create empty
	if result.Item == nil {
		return &ActiveIssues{
			PK:           "KANBAN#issues",
			SK:           "ACTIVE",
			Fingerprints: make(map[string]string),
			LastUpdated:  time.Now(),
		}, nil
	}

	var issues ActiveIssues
	if err := attributevalue.UnmarshalMap(result.Item, &issues); err != nil {
		return nil, fmt.Errorf("unmarshaling active issues: %w", err)
	}

	if issues.Fingerprints == nil {
		issues.Fingerprints = make(map[string]string)
	}

	return &issues, nil
}

// SaveActiveIssues saves the active issues fingerprint map
func (c *Client) SaveActiveIssues(ctx context.Context, issues *ActiveIssues) error {
	issues.LastUpdated = time.Now()

	av, err := attributevalue.MarshalMap(issues)
	if err != nil {
		return fmt.Errorf("marshaling active issues: %w", err)
	}

	_, err = c.dynamoDB.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(c.tableName),
		Item:      av,
	})
	if err != nil {
		return fmt.Errorf("putting active issues to DynamoDB: %w", err)
	}

	return nil
}

// GetArchive retrieves archived tasks for a specific month
func (c *Client) GetArchive(ctx context.Context, month string) (*ArchivedTasks, error) {
	result, err := c.dynamoDB.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(c.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "KANBAN#archive"},
			"SK": &types.AttributeValueMemberS{Value: month},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("getting archive from DynamoDB: %w", err)
	}

	// If doesn't exist, create empty
	if result.Item == nil {
		return &ArchivedTasks{
			PK:         "KANBAN#archive",
			SK:         month,
			Month:      month,
			Tasks:      []ArchivedCard{},
			ByPriority: make(map[string]VelocityStats),
			BySource:   make(map[string]VelocityStats),
		}, nil
	}

	var archive ArchivedTasks
	if err := attributevalue.UnmarshalMap(result.Item, &archive); err != nil {
		return nil, fmt.Errorf("unmarshaling archive: %w", err)
	}

	return &archive, nil
}

// SaveArchive saves archived tasks for a specific month
func (c *Client) SaveArchive(ctx context.Context, archive *ArchivedTasks) error {
	av, err := attributevalue.MarshalMap(archive)
	if err != nil {
		return fmt.Errorf("marshaling archive: %w", err)
	}

	_, err = c.dynamoDB.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(c.tableName),
		Item:      av,
	})
	if err != nil {
		return fmt.Errorf("putting archive to DynamoDB: %w", err)
	}

	return nil
}

// ListArchiveMonths lists all archived months
func (c *Client) ListArchiveMonths(ctx context.Context) ([]string, error) {
	result, err := c.dynamoDB.Query(ctx, &dynamodb.QueryInput{
		TableName:              aws.String(c.tableName),
		KeyConditionExpression: aws.String("PK = :pk"),
		ExpressionAttributeValues: map[string]types.AttributeValue{
			":pk": &types.AttributeValueMemberS{Value: "KANBAN#archive"},
		},
		ProjectionExpression: aws.String("SK"),
	})
	if err != nil {
		return nil, fmt.Errorf("querying archive months: %w", err)
	}

	var months []string
	for _, item := range result.Items {
		if sk, ok := item["SK"].(*types.AttributeValueMemberS); ok {
			months = append(months, sk.Value)
		}
	}

	return months, nil
}

// SaveVelocityReport saves a monthly velocity report
func (c *Client) SaveVelocityReport(ctx context.Context, report *VelocityReport) error {
	data, err := json.Marshal(report)
	if err != nil {
		return fmt.Errorf("marshaling velocity report: %w", err)
	}

	_, err = c.dynamoDB.PutItem(ctx, &dynamodb.PutItemInput{
		TableName: aws.String(c.tableName),
		Item: map[string]types.AttributeValue{
			"PK":        &types.AttributeValueMemberS{Value: "KANBAN#report"},
			"SK":        &types.AttributeValueMemberS{Value: report.Month},
			"Data":      &types.AttributeValueMemberS{Value: string(data)},
			"Timestamp": &types.AttributeValueMemberS{Value: time.Now().Format(time.RFC3339)},
		},
	})
	if err != nil {
		return fmt.Errorf("putting velocity report to DynamoDB: %w", err)
	}

	return nil
}

// GetVelocityReport retrieves a monthly velocity report
func (c *Client) GetVelocityReport(ctx context.Context, month string) (*VelocityReport, error) {
	result, err := c.dynamoDB.GetItem(ctx, &dynamodb.GetItemInput{
		TableName: aws.String(c.tableName),
		Key: map[string]types.AttributeValue{
			"PK": &types.AttributeValueMemberS{Value: "KANBAN#report"},
			"SK": &types.AttributeValueMemberS{Value: month},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("getting velocity report from DynamoDB: %w", err)
	}

	if result.Item == nil {
		return nil, nil
	}

	dataAttr, ok := result.Item["Data"].(*types.AttributeValueMemberS)
	if !ok {
		return nil, fmt.Errorf("invalid report data format")
	}

	var report VelocityReport
	if err := json.Unmarshal([]byte(dataAttr.Value), &report); err != nil {
		return nil, fmt.Errorf("unmarshaling velocity report: %w", err)
	}

	return &report, nil
}

// TableExists checks if the DynamoDB table exists
func (c *Client) TableExists(ctx context.Context) (bool, error) {
	_, err := c.dynamoDB.DescribeTable(ctx, &dynamodb.DescribeTableInput{
		TableName: aws.String(c.tableName),
	})
	if err != nil {
		return false, nil
	}
	return true, nil
}
