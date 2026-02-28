package api

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

func (jc *JourneyCenter) getNodeNameByID(ctx interface{}, journeyID, nodeID string) string {
	var nodesJSON sql.NullString
	jc.db.QueryRow("SELECT nodes FROM mailing_journeys WHERE id = $1", journeyID).Scan(&nodesJSON)
	if nodesJSON.Valid {
		var nodes []JourneyNode
		json.Unmarshal([]byte(nodesJSON.String), &nodes)
		for _, node := range nodes {
			if node.ID == nodeID {
				return getNodeName(node)
			}
		}
	}
	return nodeID
}

func getNodeName(node JourneyNode) string {
	if name, ok := node.Config["name"].(string); ok && name != "" {
		return name
	}
	if subject, ok := node.Config["subject"].(string); ok && subject != "" {
		return subject
	}
	// Default to type-based name
	switch node.Type {
	case "trigger":
		return "Journey Start"
	case "email":
		return "Send Email"
	case "delay":
		return "Wait"
	case "condition":
		return "Condition"
	case "split":
		return "A/B Split"
	case "goal":
		return "Goal"
	default:
		return strings.Title(node.Type)
	}
}

func formatJourneyDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh %dm", int(d.Hours()), int(d.Minutes())%60)
	}
	days := int(d.Hours()) / 24
	hours := int(d.Hours()) % 24
	return fmt.Sprintf("%dd %dh", days, hours)
}
