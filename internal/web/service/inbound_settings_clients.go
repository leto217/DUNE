package service

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/gary/dune/internal/database/model"
	"github.com/gary/dune/internal/util/random"

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

// inboundProtocolMap unmarshals protocol-only settings (no clients decode).
func inboundProtocolMap(inbound *model.Inbound) (map[string]any, error) {
	if inbound == nil {
		return nil, fmt.Errorf("inbound is nil")
	}
	var settings map[string]any
	if err := json.Unmarshal([]byte(inbound.ProtocolJSON()), &settings); err != nil {
		return nil, err
	}
	return settings, nil
}

// persistInboundProtocolColumns writes protocol-only JSON to both stored columns.
func persistInboundProtocolColumns(tx *gorm.DB, inboundId int, protocolJSON string) error {
	if inboundId <= 0 {
		return nil
	}
	return tx.Model(&model.Inbound{}).Where("id = ?", inboundId).Updates(map[string]any{
		"protocol_settings": protocolJSON,
		"settings":          protocolJSON,
	}).Error
}

// persistProtocolFromPayload stores protocol fields from an API payload that
// may still include clients[].
func persistProtocolFromPayload(tx *gorm.DB, inboundId int, payloadSettings string) error {
	protocol, err := model.CompactProtocolSettingsJSON(payloadSettings)
	if err != nil {
		return err
	}
	return persistInboundProtocolColumns(tx, inboundId, protocol)
}

// stripInboundSettingsClients ensures stored JSON columns never carry clients[].
func stripInboundSettingsClients(tx *gorm.DB, inboundId int) error {
	if inboundId <= 0 {
		return nil
	}
	var ib model.Inbound
	if err := tx.Select("id", "settings", "protocol_settings").First(&ib, inboundId).Error; err != nil {
		return err
	}
	protocol := ib.ProtocolJSON()
	stripped, err := model.CompactProtocolSettingsJSON(protocol)
	if err != nil {
		return err
	}
	if stripped == protocol && ib.ProtocolSettings != "" {
		return nil
	}
	return persistInboundProtocolColumns(tx, inboundId, stripped)
}

// compactSettingsWithoutClients returns settings with clients[] replaced by [].
func compactSettingsWithoutClients(settings string) (string, bool, error) {
	stripped, err := model.CompactProtocolSettingsJSON(settings)
	if err != nil {
		return settings, false, err
	}
	if stripped == settings {
		return settings, false, nil
	}
	return stripped, true, nil
}

// singleClientPayloadSettings builds API update payload JSON: protocol fields plus
// one client row (UpdateInboundClient reads clients[] from the payload only).
func singleClientPayloadSettings(inbound *model.Inbound, client model.Client) (string, error) {
	settings, err := inboundProtocolMap(inbound)
	if err != nil {
		return "", err
	}
	settings["clients"] = []model.Client{client}
	bs, err := json.Marshal(settings)
	if err != nil {
		return "", err
	}
	return string(bs), nil
}

func enrichPayloadClients(clients []model.Client) {
	nowTs := time.Now().Unix() * 1000
	for i := range clients {
		if clients[i].CreatedAt == 0 {
			clients[i].CreatedAt = nowTs
		}
		clients[i].UpdatedAt = nowTs
		if strings.TrimSpace(clients[i].SubID) == "" {
			clients[i].SubID = random.NumLower(16)
		}
	}
}

// rebuildInboundSettingsClients injects the normalized client list into an
// in-memory inbound before pushing to xray or a remote node.
func (s *InboundService) rebuildInboundSettingsClients(tx *gorm.DB, inbound *model.Inbound) error {
	if inbound == nil {
		return fmt.Errorf("inbound is nil")
	}
	settings, err := inboundProtocolMap(inbound)
	if err != nil {
		return err
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

// persistVlessTestseedFromClients updates protocol JSON when testseed should be
// dropped because no client uses xtls-rprx-vision.
func persistVlessTestseedFromClients(tx *gorm.DB, inboundId int, clients []model.Client) error {
	var ib model.Inbound
	if err := tx.Select("id", "settings", "protocol_settings", "protocol").First(&ib, inboundId).Error; err != nil {
		return err
	}
	if ib.Protocol != model.VLESS {
		return nil
	}
	protoMap, err := inboundProtocolMap(&ib)
	if err != nil {
		return err
	}
	hasVisionFlow := false
	for _, c := range clients {
		if c.Flow == "xtls-rprx-vision" {
			hasVisionFlow = true
			break
		}
	}
	if hasVisionFlow {
		return nil
	}
	delete(protoMap, "testseed")
	bs, err := json.Marshal(protoMap)
	if err != nil {
		return err
	}
	return persistInboundProtocolColumns(tx, inboundId, string(bs))
}
