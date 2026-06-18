package service

import (
	"strings"

	"github.com/gary/dune/internal/database"
	"github.com/gary/dune/internal/database/model"

	"gorm.io/gorm"
)

// upsertClientRecords find-or-creates the client_records row for every client
// (keyed by email), merging the incoming fields the same way SyncInbound does,
// and returns email→record-id. It never touches client_inbounds, so it is the
// shared record half of both the full SyncInbound rebuild and the incremental
// single-client paths.
func (s *ClientService) upsertClientRecords(tx *gorm.DB, clients []model.Client) (map[string]int, error) {
	if tx == nil {
		tx = database.GetDB()
	}

	emails := make([]string, 0, len(clients))
	seen := make(map[string]struct{}, len(clients))
	for i := range clients {
		email := strings.TrimSpace(clients[i].Email)
		if email == "" {
			continue
		}
		if _, ok := seen[email]; ok {
			continue
		}
		seen[email] = struct{}{}
		emails = append(emails, email)
	}

	existing := make(map[string]*model.ClientRecord, len(emails))
	const selectChunk = 400
	for start := 0; start < len(emails); start += selectChunk {
		end := min(start+selectChunk, len(emails))
		var rows []model.ClientRecord
		if err := tx.Where("email IN ?", emails[start:end]).Find(&rows).Error; err != nil {
			return nil, err
		}
		for i := range rows {
			r := rows[i]
			existing[r.Email] = &r
		}
	}

	idByEmail := make(map[string]int, len(emails))
	pending := make(map[string]*model.ClientRecord, len(emails))
	toCreate := make([]*model.ClientRecord, 0, len(emails))
	for i := range clients {
		email := strings.TrimSpace(clients[i].Email)
		if email == "" {
			continue
		}

		incoming := clients[i].ToRecord()
		row, ok := existing[email]
		if !ok {
			if _, dup := pending[email]; !dup {
				pending[email] = incoming
				toCreate = append(toCreate, incoming)
			}
			continue
		}

		before := *row
		if incoming.UUID != "" {
			row.UUID = incoming.UUID
		}
		if incoming.Password != "" {
			row.Password = incoming.Password
		}
		if incoming.Auth != "" {
			row.Auth = incoming.Auth
		}
		row.Flow = incoming.Flow
		if incoming.Security != "" {
			row.Security = incoming.Security
		}
		if incoming.Reverse != "" {
			row.Reverse = incoming.Reverse
		}
		row.SubID = incoming.SubID
		row.LimitIP = incoming.LimitIP
		row.TotalGB = incoming.TotalGB
		row.ExpiryTime = incoming.ExpiryTime
		row.Enable = incoming.Enable
		row.TgID = incoming.TgID
		if incoming.Group != "" {
			row.Group = incoming.Group
		}
		row.Comment = incoming.Comment
		row.Reset = incoming.Reset
		if incoming.CreatedAt > 0 && (row.CreatedAt == 0 || incoming.CreatedAt < row.CreatedAt) {
			row.CreatedAt = incoming.CreatedAt
		}
		preservedUpdatedAt := max(incoming.UpdatedAt, row.UpdatedAt)
		row.UpdatedAt = preservedUpdatedAt

		idByEmail[email] = row.Id

		if *row == before {
			continue
		}
		if err := tx.Save(row).Error; err != nil {
			return nil, err
		}
		if err := tx.Model(&model.ClientRecord{}).
			Where("id = ?", row.Id).
			UpdateColumn("updated_at", preservedUpdatedAt).Error; err != nil {
			return nil, err
		}
	}

	if len(toCreate) > 0 {
		if err := tx.CreateInBatches(toCreate, 200).Error; err != nil {
			return nil, err
		}
		for _, rec := range toCreate {
			idByEmail[rec.Email] = rec.Id
		}
	}
	return idByEmail, nil
}

// SyncInbound is the authoritative reconcile of one inbound's client_inbounds
// rows: it rebuilds the whole link set from the supplied client list. Its cost
// is O(total clients in the inbound), so single-/few-client mutations should
// prefer the incremental helpers (AttachClientsToInbound / DetachClientFrom
// Inbound) which touch only the changed rows. Keep SyncInbound for bulk writes,
// inbound updates, migrations, and node reconcile where the full list changes.
func (s *ClientService) SyncInbound(tx *gorm.DB, inboundId int, clients []model.Client) error {
	if tx == nil {
		tx = database.GetDB()
	}

	if err := tx.Where("inbound_id = ?", inboundId).Delete(&model.ClientInbound{}).Error; err != nil {
		return err
	}

	idByEmail, err := s.upsertClientRecords(tx, clients)
	if err != nil {
		return err
	}

	links := make([]model.ClientInbound, 0, len(clients))
	linked := make(map[int]struct{}, len(clients))
	for i := range clients {
		email := strings.TrimSpace(clients[i].Email)
		if email == "" {
			continue
		}
		id, ok := idByEmail[email]
		if !ok {
			continue
		}
		if _, dup := linked[id]; dup {
			continue
		}
		linked[id] = struct{}{}
		links = append(links, model.ClientInbound{
			ClientId:     id,
			InboundId:    inboundId,
			FlowOverride: clients[i].Flow,
		})
	}
	if len(links) > 0 {
		if err := tx.CreateInBatches(links, 200).Error; err != nil {
			return err
		}
	}
	return stripInboundSettingsClients(tx, inboundId)
}

// AttachClientsToInbound incrementally attaches the given clients to inboundId
// without rebuilding the inbound's whole link set. It upserts each client's
// record and the single (client,inbound) link row, leaving every other client
// of the inbound untouched. Use it on the add path where SyncInbound's full
// delete+reinsert would be O(total clients) per add. The inbound's existing
// link set must already be consistent (it is, since every mutation keeps it so,
// and reconcile paths still run the full SyncInbound).
func (s *ClientService) AttachClientsToInbound(tx *gorm.DB, inboundId int, clients []model.Client) error {
	if tx == nil {
		tx = database.GetDB()
	}
	if len(clients) == 0 {
		return nil
	}

	idByEmail, err := s.upsertClientRecords(tx, clients)
	if err != nil {
		return err
	}

	links := make([]model.ClientInbound, 0, len(clients))
	linked := make(map[int]struct{}, len(clients))
	for i := range clients {
		email := strings.TrimSpace(clients[i].Email)
		if email == "" {
			continue
		}
		id, ok := idByEmail[email]
		if !ok {
			continue
		}
		if _, dup := linked[id]; dup {
			continue
		}
		linked[id] = struct{}{}
		links = append(links, model.ClientInbound{
			ClientId:     id,
			InboundId:    inboundId,
			FlowOverride: clients[i].Flow,
		})
	}
	if len(links) == 0 {
		return nil
	}
	// Clear any pre-existing rows for exactly these clients on this inbound, then
	// insert fresh. This stays incremental (O(attached clients), never the whole
	// inbound) and — unlike ON CONFLICT — does not depend on a (client_id,
	// inbound_id) unique constraint being present, which older migrated Postgres
	// schemas may lack.
	clientIDs := make([]int, 0, len(links))
	for i := range links {
		clientIDs = append(clientIDs, links[i].ClientId)
	}
	if err := tx.Where("inbound_id = ? AND client_id IN ?", inboundId, clientIDs).
		Delete(&model.ClientInbound{}).Error; err != nil {
		return err
	}
	if err := tx.CreateInBatches(links, 200).Error; err != nil {
		return err
	}
	return stripInboundSettingsClients(tx, inboundId)
}

// DetachClientFromInbound incrementally removes a single client's link from
// inboundId, identified by email, without rebuilding the inbound's whole link
// set. The client_records row itself is left intact (callers handle record
// deletion separately, exactly as SyncInbound did — it only ever rebuilt links).
func (s *ClientService) DetachClientFromInbound(tx *gorm.DB, inboundId int, email string) error {
	if tx == nil {
		tx = database.GetDB()
	}
	email = strings.TrimSpace(email)
	if email == "" {
		return nil
	}
	clientIDs := tx.Model(&model.ClientRecord{}).Select("id").Where("email = ?", email)
	if err := tx.Where("inbound_id = ? AND client_id IN (?)", inboundId, clientIDs).
		Delete(&model.ClientInbound{}).Error; err != nil {
		return err
	}
	return stripInboundSettingsClients(tx, inboundId)
}

func (s *ClientService) DetachInbound(tx *gorm.DB, inboundId int) error {
	if tx == nil {
		tx = database.GetDB()
	}
	return tx.Where("inbound_id = ?", inboundId).Delete(&model.ClientInbound{}).Error
}

func (s *ClientService) ListForInbound(tx *gorm.DB, inboundId int) ([]model.Client, error) {
	clientsByInbound, err := s.ListForInbounds(tx, inboundId)
	if err != nil {
		return nil, err
	}
	return clientsByInbound[inboundId], nil
}

// ListForInbounds loads clients for multiple inbounds in one round-trip.
func (s *ClientService) ListForInbounds(tx *gorm.DB, inboundIds ...int) (map[int][]model.Client, error) {
	if len(inboundIds) == 0 {
		return map[int][]model.Client{}, nil
	}
	if tx == nil {
		tx = database.GetDB()
	}
	type joinedRow struct {
		model.ClientRecord
		FlowOverride string `gorm:"column:flow_override"`
		InboundId    int    `gorm:"column:inbound_id"`
	}
	var rows []joinedRow
	err := tx.Table("clients").
		Select("clients.*, client_inbounds.flow_override AS flow_override, client_inbounds.inbound_id AS inbound_id").
		Joins("JOIN client_inbounds ON client_inbounds.client_id = clients.id").
		Where("client_inbounds.inbound_id IN ?", inboundIds).
		Order("client_inbounds.inbound_id ASC, clients.id ASC").
		Find(&rows).Error
	if err != nil {
		return nil, err
	}

	out := make(map[int][]model.Client, len(inboundIds))
	for i := range rows {
		c := rows[i].ToClient()
		c.Flow = rows[i].FlowOverride
		out[rows[i].InboundId] = append(out[rows[i].InboundId], *c)
	}
	return out, nil
}
