package model

import (
	"encoding/json"
	"strings"
)

// Schema version milestones. Bump CurrentSchemaVersion when adding a new
// runSchemaMigrations step; imported databases below that version are upgraded
// on panel start.
const (
	SchemaVersionLegacy      = 1 // clients[] lived inside inbounds.settings JSON
	SchemaVersionNormalized  = 2 // clients table + inbounds.protocol_settings
	CurrentSchemaVersion     = SchemaVersionNormalized
)

// DBMeta holds panel-wide schema state. A single row (id=1) is created on first
// boot; Version drives runSchemaMigrations on startup and after DB import.
type DBMeta struct {
	Id      int `json:"id" gorm:"primaryKey"`
	Version int `json:"version" gorm:"not null;default:1"`
}

func (DBMeta) TableName() string { return "db_meta" }

// CompactProtocolSettingsJSON returns protocol-only inbound settings: every key
// except clients[] is preserved; clients is forced to [] without decoding each
// client object (uses json.RawMessage).
func CompactProtocolSettingsJSON(settings string) (string, error) {
	if strings.TrimSpace(settings) == "" {
		return `{"clients":[]}`, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(settings), &top); err != nil {
		return settings, err
	}
	top["clients"] = json.RawMessage("[]")
	out, err := json.Marshal(top)
	if err != nil {
		return settings, err
	}
	return string(out), nil
}

// ProtocolJSON returns the stored protocol-level settings JSON for this inbound.
// After schema v2 this is protocol_settings; legacy rows fall back to a compacted
// settings column (never the fat clients[] blob).
func (i *Inbound) ProtocolJSON() string {
	if i == nil {
		return ""
	}
	if ps := strings.TrimSpace(i.ProtocolSettings); ps != "" {
		return ps
	}
	if compact, err := CompactProtocolSettingsJSON(i.Settings); err == nil {
		return compact
	}
	return i.Settings
}
