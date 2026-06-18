package database

import (
	"log"

	"github.com/gary/dune/internal/database/model"

	"gorm.io/gorm"
)

func runSchemaMigrations() error {
	if err := ensureDBMetaRow(); err != nil {
		return err
	}
	var meta model.DBMeta
	if err := db.First(&meta, 1).Error; err != nil {
		return err
	}
	for meta.Version < model.CurrentSchemaVersion {
		switch meta.Version {
		case model.SchemaVersionLegacy:
			if err := migrateSchemaV1ToV2(); err != nil {
				return err
			}
			meta.Version = model.SchemaVersionNormalized
		default:
			log.Printf("schema: unknown version %d, bumping to %d", meta.Version, model.CurrentSchemaVersion)
			meta.Version = model.CurrentSchemaVersion
		}
		if err := db.Model(&model.DBMeta{}).Where("id = 1").Update("version", meta.Version).Error; err != nil {
			return err
		}
	}
	return nil
}

func ensureDBMetaRow() error {
	var count int64
	if err := db.Model(&model.DBMeta{}).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return nil
	}
	return db.Create(&model.DBMeta{Id: 1, Version: model.SchemaVersionLegacy}).Error
}

// GetSchemaVersion returns the persisted schema version (1 when db_meta is missing).
func GetSchemaVersion() int {
	if db == nil {
		return model.SchemaVersionLegacy
	}
	var meta model.DBMeta
	if err := db.First(&meta, 1).Error; err != nil {
		return model.SchemaVersionLegacy
	}
	return meta.Version
}

// migrateSchemaV1ToV2 backfills normalized clients from legacy settings JSON,
// then moves protocol fields into protocol_settings and strips clients[] from
// both stored JSON columns. Safe to run on an already-normalized DB (no-op
// strips).
func migrateSchemaV1ToV2() error {
	log.Printf("schema: migrating v%d -> v%d (clients out of inbounds.settings)", model.SchemaVersionLegacy, model.SchemaVersionNormalized)

	if err := seedClientsFromInboundJSON(); err != nil {
		return err
	}

	var inbounds []model.Inbound
	if err := db.Select("id", "settings").Find(&inbounds).Error; err != nil {
		return err
	}

	return db.Transaction(func(tx *gorm.DB) error {
		for _, inbound := range inbounds {
			protocol, err := model.CompactProtocolSettingsJSON(inbound.Settings)
			if err != nil {
				log.Printf("schema v2: skip inbound %d compact: %v", inbound.Id, err)
				continue
			}
			if err := tx.Model(&model.Inbound{}).Where("id = ?", inbound.Id).Updates(map[string]any{
				"protocol_settings": protocol,
				"settings":          protocol,
			}).Error; err != nil {
				return err
			}
		}

		// Mark ClientsTable seeder done so runSeeders does not repeat the backfill.
		var existing model.HistoryOfSeeders
		res := tx.Where("seeder_name = ?", "ClientsTable").First(&existing)
		if res.Error != nil && res.Error != gorm.ErrRecordNotFound {
			return res.Error
		}
		if res.Error == gorm.ErrRecordNotFound {
			if err := tx.Create(&model.HistoryOfSeeders{SeederName: "ClientsTable"}).Error; err != nil {
				return err
			}
		}

		var slimSeeder model.HistoryOfSeeders
		res = tx.Where("seeder_name = ?", "SlimInboundSettingsClients").First(&slimSeeder)
		if res.Error != nil && res.Error != gorm.ErrRecordNotFound {
			return res.Error
		}
		if res.Error == gorm.ErrRecordNotFound {
			if err := tx.Create(&model.HistoryOfSeeders{SeederName: "SlimInboundSettingsClients"}).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

