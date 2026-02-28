package ongage

import (
	"encoding/json"
	"testing"
)

func TestFlexString_UnmarshalJSON_String(t *testing.T) {
	jsonData := `{"value": "123"}`
	
	var result struct {
		Value FlexString `json:"value"`
	}
	
	err := json.Unmarshal([]byte(jsonData), &result)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	
	if result.Value.String() != "123" {
		t.Errorf("Expected '123', got '%s'", result.Value.String())
	}
}

func TestFlexString_UnmarshalJSON_Number(t *testing.T) {
	jsonData := `{"value": 456}`
	
	var result struct {
		Value FlexString `json:"value"`
	}
	
	err := json.Unmarshal([]byte(jsonData), &result)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	
	if result.Value.String() != "456" {
		t.Errorf("Expected '456', got '%s'", result.Value.String())
	}
}

func TestFlexString_UnmarshalJSON_Float(t *testing.T) {
	jsonData := `{"value": 7.89}`
	
	var result struct {
		Value FlexString `json:"value"`
	}
	
	err := json.Unmarshal([]byte(jsonData), &result)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	
	if result.Value.String() != "7.89" {
		t.Errorf("Expected '7.89', got '%s'", result.Value.String())
	}
}

func TestFlexString_UnmarshalJSON_Empty(t *testing.T) {
	jsonData := `{"value": ""}`
	
	var result struct {
		Value FlexString `json:"value"`
	}
	
	err := json.Unmarshal([]byte(jsonData), &result)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	
	if result.Value.String() != "" {
		t.Errorf("Expected empty string, got '%s'", result.Value.String())
	}
}

func TestESPDistribution_UnmarshalJSON_MixedTypes(t *testing.T) {
	// Test that ESPDistribution correctly handles both string and number types for esp_id
	jsonData := `{
		"esp_id": 4,
		"esp_connection_id": "100",
		"domain": "mail.example.com",
		"percent": "100"
	}`
	
	var dist ESPDistribution
	err := json.Unmarshal([]byte(jsonData), &dist)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	
	if dist.ESPID.String() != "4" {
		t.Errorf("Expected ESPID '4', got '%s'", dist.ESPID.String())
	}
	
	if dist.ESPConnectionID.String() != "100" {
		t.Errorf("Expected ESPConnectionID '100', got '%s'", dist.ESPConnectionID.String())
	}
	
	if dist.Domain != "mail.example.com" {
		t.Errorf("Expected Domain 'mail.example.com', got '%s'", dist.Domain)
	}
}

func TestESPDistribution_UnmarshalJSON_AllNumbers(t *testing.T) {
	// Test that ESPDistribution handles all numeric values
	jsonData := `{
		"esp_id": 4,
		"esp_connection_id": 100,
		"isp_id": 1,
		"percent": 100,
		"segment_id": 5001
	}`
	
	var dist ESPDistribution
	err := json.Unmarshal([]byte(jsonData), &dist)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	
	if dist.ESPID.String() != "4" {
		t.Errorf("Expected ESPID '4', got '%s'", dist.ESPID.String())
	}
	
	if dist.ESPConnectionID.String() != "100" {
		t.Errorf("Expected ESPConnectionID '100', got '%s'", dist.ESPConnectionID.String())
	}
	
	if dist.ISPID.String() != "1" {
		t.Errorf("Expected ISPID '1', got '%s'", dist.ISPID.String())
	}
	
	if dist.Percent.String() != "100" {
		t.Errorf("Expected Percent '100', got '%s'", dist.Percent.String())
	}
	
	if dist.SegmentID.String() != "5001" {
		t.Errorf("Expected SegmentID '5001', got '%s'", dist.SegmentID.String())
	}
}

func TestESPConnection_UnmarshalJSON(t *testing.T) {
	// Test that ESPConnection handles both string and number for esp_id
	jsonData := `{
		"id": "1",
		"esp_id": 4,
		"name": "SparkPost Production"
	}`
	
	var conn ESPConnection
	err := json.Unmarshal([]byte(jsonData), &conn)
	if err != nil {
		t.Fatalf("Expected no error, got: %v", err)
	}
	
	if conn.ID != "1" {
		t.Errorf("Expected ID '1', got '%s'", conn.ID)
	}
	
	if conn.ESPID.String() != "4" {
		t.Errorf("Expected ESPID '4', got '%s'", conn.ESPID.String())
	}
	
	if conn.Name != "SparkPost Production" {
		t.Errorf("Expected Name 'SparkPost Production', got '%s'", conn.Name)
	}
}
