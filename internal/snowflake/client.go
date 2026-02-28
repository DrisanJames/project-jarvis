package snowflake

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/snowflakedb/gosnowflake" // Snowflake driver
)

// Client provides access to Snowflake database
type Client struct {
	config Config
	db     *sql.DB
}

// NewClient creates a new Snowflake client
func NewClient(cfg Config) (*Client, error) {
	// Build DSN (Data Source Name)
	// Format: user:password@account/database/schema?warehouse=xxx
	dsn := fmt.Sprintf("%s:%s@%s/%s/%s",
		cfg.User,
		cfg.Password,
		cfg.Account,
		cfg.Database,
		cfg.Schema,
	)
	
	if cfg.Warehouse != "" {
		dsn += "?warehouse=" + cfg.Warehouse
	}
	
	db, err := sql.Open("snowflake", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open snowflake connection: %w", err)
	}
	
	// Set connection pool settings
	db.SetMaxOpenConns(5)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(5 * time.Minute)
	
	return &Client{
		config: cfg,
		db:     db,
	}, nil
}

// Close closes the database connection
func (c *Client) Close() error {
	if c.db != nil {
		return c.db.Close()
	}
	return nil
}

// Ping tests the database connection
func (c *Client) Ping(ctx context.Context) error {
	return c.db.PingContext(ctx)
}

// GetTotalRecordCount returns the total number of records in SUBSCRIBER_VALIDATIONS_EO
func (c *Client) GetTotalRecordCount(ctx context.Context) (int64, error) {
	query := `SELECT COUNT(*) FROM SUBSCRIBER_VALIDATIONS_EO`
	
	var count int64
	err := c.db.QueryRowContext(ctx, query).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to get total count: %w", err)
	}
	
	return count, nil
}

// GetTodayRecordCount returns the number of records created today
func (c *Client) GetTodayRecordCount(ctx context.Context) (int64, error) {
	today := time.Now().Format("2006-01-02")
	query := `SELECT COUNT(*) FROM SUBSCRIBER_VALIDATIONS_EO WHERE CREATIONDATE LIKE ?`
	
	var count int64
	err := c.db.QueryRowContext(ctx, query, today+"%").Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to get today count: %w", err)
	}
	
	return count, nil
}

// GetValidationStatusCounts returns counts grouped by validation status
func (c *Client) GetValidationStatusCounts(ctx context.Context) ([]ValidationStatus, error) {
	query := `
		SELECT VALIDATIONSTATUSID, COUNT(*) as cnt
		FROM SUBSCRIBER_VALIDATIONS_EO
		GROUP BY VALIDATIONSTATUSID
		ORDER BY cnt DESC
	`
	
	rows, err := c.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to get status counts: %w", err)
	}
	defer rows.Close()
	
	var result []ValidationStatus
	for rows.Next() {
		var status ValidationStatus
		if err := rows.Scan(&status.StatusID, &status.Count); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		result = append(result, status)
	}
	
	return result, nil
}

// GetDailyValidationCounts returns daily counts grouped by validation status for the last N days
func (c *Client) GetDailyValidationCounts(ctx context.Context, days int) ([]DailyValidationMetrics, error) {
	// Get the start date
	startDate := time.Now().AddDate(0, 0, -days).Format("2006-01-02")
	
	query := `
		SELECT 
			SUBSTR(CREATIONDATE, 1, 10) as date,
			VALIDATIONSTATUSID,
			COUNT(*) as cnt
		FROM SUBSCRIBER_VALIDATIONS_EO
		WHERE CREATIONDATE >= ?
		GROUP BY SUBSTR(CREATIONDATE, 1, 10), VALIDATIONSTATUSID
		ORDER BY date DESC, cnt DESC
	`
	
	rows, err := c.db.QueryContext(ctx, query, startDate)
	if err != nil {
		return nil, fmt.Errorf("failed to get daily counts: %w", err)
	}
	defer rows.Close()
	
	// Aggregate by date
	dailyMap := make(map[string]*DailyValidationMetrics)
	
	for rows.Next() {
		var date, statusID string
		var count int64
		
		if err := rows.Scan(&date, &statusID, &count); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		
		if dailyMap[date] == nil {
			dailyMap[date] = &DailyValidationMetrics{
				Date:            date,
				StatusBreakdown: []ValidationStatus{},
			}
		}
		
		dailyMap[date].TotalRecords += count
		dailyMap[date].StatusBreakdown = append(dailyMap[date].StatusBreakdown, ValidationStatus{
			StatusID: statusID,
			Count:    count,
		})
	}
	
	// Convert to slice and sort
	var result []DailyValidationMetrics
	for _, metrics := range dailyMap {
		result = append(result, *metrics)
	}
	
	// Sort by date descending
	for i := 0; i < len(result)-1; i++ {
		for j := i + 1; j < len(result); j++ {
			if result[i].Date < result[j].Date {
				result[i], result[j] = result[j], result[i]
			}
		}
	}
	
	return result, nil
}

// GetDomainGroupCounts returns counts grouped by email domain group
func (c *Client) GetDomainGroupCounts(ctx context.Context) ([]DomainGroupMetrics, error) {
	query := `
		SELECT 
			COALESCE(EMAILDOMAINGROUP, 'Unknown') as domain_group,
			COALESCE(EMAILDOMAINGROUPSHORT, 'UNK') as domain_group_short,
			COUNT(*) as cnt
		FROM SUBSCRIBER_VALIDATIONS_EO
		GROUP BY EMAILDOMAINGROUP, EMAILDOMAINGROUPSHORT
		ORDER BY cnt DESC
	`
	
	rows, err := c.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("failed to get domain group counts: %w", err)
	}
	defer rows.Close()
	
	var result []DomainGroupMetrics
	for rows.Next() {
		var metrics DomainGroupMetrics
		if err := rows.Scan(&metrics.DomainGroup, &metrics.DomainGroupShort, &metrics.Count); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		result = append(result, metrics)
	}
	
	return result, nil
}

// GetTodayStatusCounts returns today's counts grouped by validation status
func (c *Client) GetTodayStatusCounts(ctx context.Context) ([]ValidationStatus, error) {
	today := time.Now().Format("2006-01-02")
	
	query := `
		SELECT VALIDATIONSTATUSID, COUNT(*) as cnt
		FROM SUBSCRIBER_VALIDATIONS_EO
		WHERE CREATIONDATE LIKE ?
		GROUP BY VALIDATIONSTATUSID
		ORDER BY cnt DESC
	`
	
	rows, err := c.db.QueryContext(ctx, query, today+"%")
	if err != nil {
		return nil, fmt.Errorf("failed to get today status counts: %w", err)
	}
	defer rows.Close()
	
	var result []ValidationStatus
	for rows.Next() {
		var status ValidationStatus
		if err := rows.Scan(&status.StatusID, &status.Count); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		result = append(result, status)
	}
	
	return result, nil
}

// GetUniqueStatusCount returns the number of unique validation statuses
func (c *Client) GetUniqueStatusCount(ctx context.Context) (int, error) {
	query := `SELECT COUNT(DISTINCT VALIDATIONSTATUSID) FROM SUBSCRIBER_VALIDATIONS_EO`
	
	var count int
	err := c.db.QueryRowContext(ctx, query).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("failed to get unique status count: %w", err)
	}
	
	return count, nil
}
