package service

import (
	"encoding/json"
	"fmt"

	"github.com/gary/dune/internal/database/model"

	"gorm.io/gorm"
)

// parseClientsFromSettings reads clients[] from a wire-format or legacy stored
// settings JSON blob. Prefer GetClients / ListForInbound for panel-owned rows.
func parseClientsFromSettings(settings string) ([]model.Client, error) {
	if settings == "" {
		return nil, nil
	}
	var parsed map[string][]model.Client
	if err := json.Unmarshal([]byte(settings), &parsed); err != nil {
		return nil, err
	}
	if parsed == nil {
		return nil, fmt.Errorf("setting is null")
	}
	return parsed["clients"], nil
}

// stripInboundSettingsClients rewrites inbounds.settings so clients[] is empty.
// Client credentials live in the clients / client_inbounds tables; keeping a
// copy in settings made every list/sub/config path json.Unmarshal the full
// client list (tens of MB on busy panels).
func stripInboundSettingsClients(tx *gorm.DB, inboundId int) error {
	if inboundId <= 0 {
		return nil
	}
	var ib model.Inbound
	if err := tx.Select("id", "settings").First(&ib, inboundId).Error; err != nil {
		return err
	}
	stripped, changed, err := compactSettingsWithoutClients(ib.Settings)
	if err != nil || !changed {
		return err
	}
	return tx.Model(&model.Inbound{}).Where("id = ?", inboundId).Update("settings", stripped).Error
}

// compactSettingsWithoutClients returns settings with clients[] replaced by [].
// Uses json.RawMessage so nested client objects are not decoded into maps.
func compactSettingsWithoutClients(settings string) (string, bool, error) {
	if settings == "" {
		return `{"clients":[]}`, true, nil
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(settings), &top); err != nil {
		return settings, false, err
	}
	if raw, ok := top["clients"]; ok && string(raw) == "[]" {
		return settings, false, nil
	}
	top["clients"] = json.RawMessage("[]")
	out, err := json.Marshal(top)
	if err != nil {
		return settings, false, err
	}
	return string(out), true, nil
}

// rebuildInboundSettingsClients injects the normalized client list into an
// in-memory inbound before pushing to xray or a remote node. Stored settings
// stay slim; this is only for runtime/wire snapshots.
func (s *InboundService) rebuildInboundSettingsClients(tx *gorm.DB, inbound *model.Inbound) error {
	if inbound == nil {
		return fmt.Errorf("inbound is nil")
	}
	settings := map[string]any{}
	if inbound.Settings != "" {
		if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
			return err
		}
	}
	clients, err := s.clientService.ListForInbound(tx, inbound.Id)
	if err != nil {
		return err
	}
	clientRows := make([]any, 0, len(clients))
	for i := range clients {
		clientRows = append(clientRows, clients[i])
	}
	settings["clients"] = clientRows
	bs, err := json.Marshal(settings)
	if err != nil {
		return err
	}
	inbound.Settings = string(bs)
	return nil
}
