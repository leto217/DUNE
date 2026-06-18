package sub

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gary/dune/internal/database"
	"github.com/gary/dune/internal/database/model"
	"github.com/gary/dune/internal/web/service"
)

func TestGetSubs_LimitsInboundsRandomly(t *testing.T) {
	dbDir := t.TempDir()
	t.Setenv("DUNE_DB_FOLDER", dbDir)
	if err := database.InitDB(filepath.Join(dbDir, "dune.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })

	const subId = "sub-max"
	const email = "user@example.com"
	const uuid = "00000000-0000-4000-8000-000000000001"
	db := database.GetDB()

	client := &model.ClientRecord{Email: email, SubID: subId, UUID: uuid, Enable: true}
	if err := db.Create(client).Error; err != nil {
		t.Fatalf("seed client: %v", err)
	}

	for i := 1; i <= 5; i++ {
		tag := fmt.Sprintf("ib-%d", i)
		settings := fmt.Sprintf(`{"clients": [{"id": %q, "email": %q, "subId": %q, "enable": true}]}`, uuid, email, subId)
		ib := &model.Inbound{
			UserId:         1,
			Tag:            tag,
			Enable:         true,
			Port:           43000 + i,
			Protocol:       model.VLESS,
			Settings:       settings,
			StreamSettings: `{"network": "tcp", "security": "none"}`,
		}
		if err := db.Create(ib).Error; err != nil {
			t.Fatalf("seed inbound %s: %v", tag, err)
		}
		if err := db.Create(&model.ClientInbound{ClientId: client.Id, InboundId: ib.Id}).Error; err != nil {
			t.Fatalf("seed client_inbound %s: %v", tag, err)
		}
	}

	settingSvc := service.SettingService{}
	if err := settingSvc.SetSubMaxInbounds(2); err != nil {
		t.Fatalf("SetSubMaxInbounds: %v", err)
	}

	s := NewSubService("")
	s.settingService = settingSvc

	links, _, _, _, err := s.GetSubs(subId, "sub.example.com")
	if err != nil {
		t.Fatalf("GetSubs: %v", err)
	}
	if len(links) != 2 {
		t.Fatalf("links = %d, want 2 when subMaxInbounds=2", len(links))
	}

	seen := make(map[string]struct{})
	for _, link := range links {
		for i := 1; i <= 5; i++ {
			port := fmt.Sprintf(":%d", 43000+i)
			if strings.Contains(link, port) {
				seen[port] = struct{}{}
			}
		}
	}
	if len(seen) != 2 {
		t.Fatalf("expected 2 distinct inbound links, matched %d ports", len(seen))
	}
}

func TestGetSubs_SubMaxInboundsZeroShowsAll(t *testing.T) {
	dbDir := t.TempDir()
	t.Setenv("DUNE_DB_FOLDER", dbDir)
	if err := database.InitDB(filepath.Join(dbDir, "dune.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })

	const subId = "sub-all"
	db := database.GetDB()

	for i := 1; i <= 3; i++ {
		tag := fmt.Sprintf("all-%d", i)
		email := fmt.Sprintf("all%d@example.com", i)
		uuid := fmt.Sprintf("10000000-0000-4000-8000-%012d", i)
		settings := fmt.Sprintf(`{"clients": [{"id": %q, "email": %q, "subId": %q, "enable": true}]}`, uuid, email, subId)
		ib := &model.Inbound{
			UserId:         1,
			Tag:            tag,
			Enable:         true,
			Port:           44000 + i,
			Protocol:       model.VLESS,
			Settings:       settings,
			StreamSettings: `{"network": "tcp", "security": "none"}`,
			SubSortIndex:   i,
		}
		if err := db.Create(ib).Error; err != nil {
			t.Fatalf("seed inbound %s: %v", tag, err)
		}
		client := &model.ClientRecord{Email: email, SubID: subId, UUID: uuid, Enable: true}
		if err := db.Create(client).Error; err != nil {
			t.Fatalf("seed client %s: %v", email, err)
		}
		if err := db.Create(&model.ClientInbound{ClientId: client.Id, InboundId: ib.Id}).Error; err != nil {
			t.Fatalf("seed client_inbound %s: %v", email, err)
		}
	}

	s := NewSubService("")
	links, emails, _, _, err := s.GetSubs(subId, "sub.example.com")
	if err != nil {
		t.Fatalf("GetSubs: %v", err)
	}
	if len(links) != 3 {
		t.Fatalf("links = %d, want 3 when subMaxInbounds defaults to 0", len(links))
	}
	want := []string{"all1@example.com", "all2@example.com", "all3@example.com"}
	for i, email := range want {
		if emails[i] != email {
			t.Fatalf("emails order = %v, want %v (sub_sort_index ASC when unlimited)", emails, want)
		}
	}
}
