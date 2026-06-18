package service

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/gary/dune/internal/database/model"
	"github.com/gary/dune/internal/eventbus"
	"github.com/gary/dune/internal/logger"
	"github.com/gary/dune/internal/xray"

	"gorm.io/gorm"
)

// FupStatus is the computed fair-usage snapshot returned to the clients UI.
type FupStatus struct {
	DailyUsed       int64  `json:"dailyUsed"`
	DailyLimit      int64  `json:"dailyLimit"`
	WeeklyUsed      int64  `json:"weeklyUsed"`
	WeeklyLimit     int64  `json:"weeklyLimit"`
	MonthlyUsed     int64  `json:"monthlyUsed"`
	MonthlyLimit    int64  `json:"monthlyLimit"`
	Status          string `json:"status"` // normal, exceeded, disabled
	DisabledUntil   int64  `json:"disabledUntil,omitempty"`
	TriggerPeriod   string `json:"triggerPeriod,omitempty"`
}

func normalizeFupAction(action string) string {
	switch strings.TrimSpace(action) {
	case model.FupActionDisableHours, model.FupActionDisableUntilReset:
		return strings.TrimSpace(action)
	default:
		return model.FupActionNotify
	}
}

func normalizeFupResetTime(raw string) (hour, minute int) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, 0
	}
	parts := strings.Split(raw, ":")
	if len(parts) != 2 {
		return 0, 0
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil || h < 0 || h > 23 {
		h = 0
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil || m < 0 || m > 59 {
		m = 0
	}
	return h, m
}

func fupPeriodStart(now time.Time, resetTime string, kind string, weekDay, monthDay int) time.Time {
	hour, minute := normalizeFupResetTime(resetTime)
	loc := now.Location()
	candidate := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, loc)

	switch kind {
	case model.FupPeriodWeekly:
		wd := weekDay % 7
		if wd < 0 {
			wd = 1
		}
		delta := (int(now.Weekday()) - wd + 7) % 7
		candidate = candidate.AddDate(0, 0, -delta)
	case model.FupPeriodMonthly:
		day := monthDay
		if day < 1 {
			day = 1
		}
		if day > 28 {
			day = 28
		}
		candidate = time.Date(now.Year(), now.Month(), day, hour, minute, 0, 0, loc)
		if now.Before(candidate) {
			candidate = candidate.AddDate(0, -1, 0)
		}
	default:
		if now.Before(candidate) {
			candidate = candidate.AddDate(0, 0, -1)
		}
	}
	return candidate
}

func clientTotalUsage(traffic *xray.ClientTraffic) int64 {
	if traffic == nil {
		return 0
	}
	return traffic.Up + traffic.Down
}

func isClientQuotaDepleted(traffic *xray.ClientTraffic, nowMs int64) bool {
	if traffic == nil {
		return false
	}
	if traffic.Total > 0 && clientTotalUsage(traffic) >= traffic.Total {
		return true
	}
	if traffic.ExpiryTime > 0 && traffic.ExpiryTime <= nowMs {
		return true
	}
	return false
}

func computeFupStatus(rec *model.ClientRecord, state *model.ClientFupState, traffic *xray.ClientTraffic, now time.Time) *FupStatus {
	if rec == nil || !rec.HasFairUsagePolicy() {
		return nil
	}
	usage := clientTotalUsage(traffic)
	if state == nil {
		state = &model.ClientFupState{Email: rec.Email}
	}

	status := &FupStatus{
		DailyLimit:   rec.FupDailyLimitGB,
		WeeklyLimit:  rec.FupWeeklyLimitGB,
		MonthlyLimit: rec.FupMonthlyLimitGB,
		Status:       "normal",
	}
	resetTime := rec.FupResetTime

	if rec.FupDailyLimitGB > 0 {
		status.DailyUsed = usage - state.DailyBaseline
		if status.DailyUsed < 0 {
			status.DailyUsed = 0
		}
		if status.DailyUsed > rec.FupDailyLimitGB {
			status.Status = "exceeded"
		}
	}
	if rec.FupWeeklyLimitGB > 0 {
		status.WeeklyUsed = usage - state.WeeklyBaseline
		if status.WeeklyUsed < 0 {
			status.WeeklyUsed = 0
		}
		if status.WeeklyUsed > rec.FupWeeklyLimitGB {
			status.Status = "exceeded"
		}
	}
	if rec.FupMonthlyLimitGB > 0 {
		status.MonthlyUsed = usage - state.MonthlyBaseline
		if status.MonthlyUsed < 0 {
			status.MonthlyUsed = 0
		}
		if status.MonthlyUsed > rec.FupMonthlyLimitGB {
			status.Status = "exceeded"
		}
	}

	nowMs := now.UnixMilli()
	if state.FupDisabledByFup && traffic != nil && !traffic.Enable {
		status.Status = "disabled"
		status.TriggerPeriod = state.FupTriggerPeriod
		if state.FupDisabledUntil > nowMs {
			status.DisabledUntil = state.FupDisabledUntil
		} else if state.FupTriggerPeriod != "" {
			switch state.FupTriggerPeriod {
			case model.FupPeriodDaily:
				next := fupPeriodStart(now, resetTime, model.FupPeriodDaily, 0, 0).AddDate(0, 0, 1)
				status.DisabledUntil = next.UnixMilli()
			case model.FupPeriodWeekly:
				next := fupPeriodStart(now, resetTime, model.FupPeriodWeekly, rec.FupWeeklyResetDay, 0).AddDate(0, 0, 7)
				status.DisabledUntil = next.UnixMilli()
			case model.FupPeriodMonthly:
				next := fupPeriodStart(now, resetTime, model.FupPeriodMonthly, 0, rec.FupMonthlyResetDay).AddDate(0, 1, 0)
				status.DisabledUntil = next.UnixMilli()
			}
		}
	} else if status.Status == "exceeded" {
		// keep exceeded when over limit but still enabled (notify-only action)
	}

	_ = resetTime
	return status
}

type fupCandidate struct {
	rec     model.ClientRecord
	state   model.ClientFupState
	traffic xray.ClientTraffic
	period  string
	used    int64
	limit   int64
}

func (s *InboundService) enforceFairUsage(tx *gorm.DB) (bool, bool, []int, error) {
	now := time.Now()
	nowMs := now.UnixMilli()

	var records []model.ClientRecord
	err := tx.Where("fup_daily_limit_gb > 0 OR fup_weekly_limit_gb > 0 OR fup_monthly_limit_gb > 0").
		Find(&records).Error
	if err != nil {
		return false, false, nil, err
	}
	if len(records) == 0 {
		return false, false, nil, nil
	}

	emails := make([]string, 0, len(records))
	for i := range records {
		if records[i].Email != "" {
			emails = append(emails, records[i].Email)
		}
	}

	trafficByEmail := make(map[string]xray.ClientTraffic, len(emails))
	for _, batch := range chunkStrings(emails, sqlInChunk) {
		var batchStats []xray.ClientTraffic
		if err := tx.Where("email IN ?", batch).Find(&batchStats).Error; err != nil {
			return false, false, nil, err
		}
		for i := range batchStats {
			trafficByEmail[batchStats[i].Email] = batchStats[i]
		}
	}
	ptrStats := make([]*xray.ClientTraffic, 0, len(trafficByEmail))
	for email := range trafficByEmail {
		t := trafficByEmail[email]
		ptrStats = append(ptrStats, &t)
	}
	overlayGlobalTraffic(tx, ptrStats)
	for _, t := range ptrStats {
		if t != nil {
			trafficByEmail[t.Email] = *t
		}
	}

	stateByEmail := make(map[string]model.ClientFupState, len(emails))
	for _, batch := range chunkStrings(emails, sqlInChunk) {
		var batchStates []model.ClientFupState
		if err := tx.Where("email IN ?", batch).Find(&batchStates).Error; err != nil {
			return false, false, nil, err
		}
		for i := range batchStates {
			stateByEmail[batchStates[i].Email] = batchStates[i]
		}
	}

	needRestart := false
	clientsDisabled := false
	var disabledNodeIDs []int
	var toDisable []string
	var toEnable []string
	stateDirty := make(map[string]model.ClientFupState)

	for i := range records {
		rec := records[i]
		traffic, hasTraffic := trafficByEmail[rec.Email]
		if !hasTraffic {
			continue
		}

		state := stateByEmail[rec.Email]
		if state.Email == "" {
			state.Email = rec.Email
		}
		usage := clientTotalUsage(&traffic)
		resetTime := rec.FupResetTime

		// Period rollover: reset baselines and maybe re-enable FUP-disabled clients.
		if rec.FupDailyLimitGB > 0 {
			start := fupPeriodStart(now, resetTime, model.FupPeriodDaily, 0, 0).UnixMilli()
			if state.DailyPeriodStart != start {
				state.DailyBaseline = usage
				state.DailyPeriodStart = start
				state.DailyNotified = false
				if state.FupDisabledByFup && state.FupTriggerPeriod == model.FupPeriodDaily &&
					normalizeFupAction(rec.FupAction) == model.FupActionDisableUntilReset {
					toEnable = append(toEnable, rec.Email)
					state.FupDisabledByFup = false
					state.FupTriggerPeriod = ""
					state.FupDisabledUntil = 0
				}
			}
		}
		if rec.FupWeeklyLimitGB > 0 {
			start := fupPeriodStart(now, resetTime, model.FupPeriodWeekly, rec.FupWeeklyResetDay, 0).UnixMilli()
			if state.WeeklyPeriodStart != start {
				state.WeeklyBaseline = usage
				state.WeeklyPeriodStart = start
				state.WeeklyNotified = false
				if state.FupDisabledByFup && state.FupTriggerPeriod == model.FupPeriodWeekly &&
					normalizeFupAction(rec.FupAction) == model.FupActionDisableUntilReset {
					toEnable = append(toEnable, rec.Email)
					state.FupDisabledByFup = false
					state.FupTriggerPeriod = ""
					state.FupDisabledUntil = 0
				}
			}
		}
		if rec.FupMonthlyLimitGB > 0 {
			start := fupPeriodStart(now, resetTime, model.FupPeriodMonthly, 0, rec.FupMonthlyResetDay).UnixMilli()
			if state.MonthlyPeriodStart != start {
				state.MonthlyBaseline = usage
				state.MonthlyPeriodStart = start
				state.MonthlyNotified = false
				if state.FupDisabledByFup && state.FupTriggerPeriod == model.FupPeriodMonthly &&
					normalizeFupAction(rec.FupAction) == model.FupActionDisableUntilReset {
					toEnable = append(toEnable, rec.Email)
					state.FupDisabledByFup = false
					state.FupTriggerPeriod = ""
					state.FupDisabledUntil = 0
				}
			}
		}

		// disable_hours expiry re-enable
		if state.FupDisabledByFup && state.FupDisabledUntil > 0 && nowMs >= state.FupDisabledUntil &&
			normalizeFupAction(rec.FupAction) == model.FupActionDisableHours {
			toEnable = append(toEnable, rec.Email)
			state.FupDisabledByFup = false
			state.FupTriggerPeriod = ""
			state.FupDisabledUntil = 0
		}

		stateDirty[rec.Email] = state

		if !traffic.Enable || isClientQuotaDepleted(&traffic, nowMs) {
			continue
		}
		if state.FupDisabledByFup {
			continue
		}

		var exceeded []fupCandidate
		check := func(period string, limit, baseline int64, notified *bool) {
			if limit <= 0 {
				return
			}
			used := usage - baseline
			if used < 0 {
				used = 0
			}
			if used > limit {
				exceeded = append(exceeded, fupCandidate{
					rec: rec, state: state, traffic: traffic,
					period: period, used: used, limit: limit,
				})
				if normalizeFupAction(rec.FupAction) == model.FupActionNotify && !*notified {
					publishFupExceeded(rec.Email, period, used, limit, rec.FupAction)
					*notified = true
				}
			}
		}
		check(model.FupPeriodDaily, rec.FupDailyLimitGB, state.DailyBaseline, &state.DailyNotified)
		check(model.FupPeriodWeekly, rec.FupWeeklyLimitGB, state.WeeklyBaseline, &state.WeeklyNotified)
		check(model.FupPeriodMonthly, rec.FupMonthlyLimitGB, state.MonthlyBaseline, &state.MonthlyNotified)
		stateDirty[rec.Email] = state

		if len(exceeded) == 0 {
			continue
		}

		action := normalizeFupAction(rec.FupAction)
		if action == model.FupActionNotify {
			continue
		}

		// Pick the period with the highest overage ratio for disable tracking.
		worst := exceeded[0]
		for _, c := range exceeded[1:] {
			if float64(c.used)/float64(c.limit) > float64(worst.used)/float64(worst.limit) {
				worst = c
			}
		}

		st := stateDirty[rec.Email]
		st.FupDisabledByFup = true
		st.FupTriggerPeriod = worst.period
		if action == model.FupActionDisableHours {
			hours := rec.FupDisableHours
			if hours <= 0 {
				hours = 1
			}
			st.FupDisabledUntil = nowMs + int64(hours)*3600000
		} else {
			st.FupDisabledUntil = 0
		}
		stateDirty[rec.Email] = st
		toDisable = append(toDisable, rec.Email)
	}

	for email, st := range stateDirty {
		if err := tx.Save(&st).Error; err != nil {
			logger.Warning("enforceFairUsage save state:", email, err)
		}
	}

	if len(toEnable) > 0 {
		nr, nodeIDs, eErr := s.enableClientsByEmails(tx, toEnable, nowMs)
		if eErr != nil {
			logger.Warning("enforceFairUsage re-enable:", eErr)
		}
		if nr {
			needRestart = true
		}
		disabledNodeIDs = append(disabledNodeIDs, nodeIDs...)
	}

	if len(toDisable) > 0 {
		seen := make(map[string]struct{}, len(toDisable))
		unique := make([]string, 0, len(toDisable))
		for _, e := range toDisable {
			if _, ok := seen[e]; ok {
				continue
			}
			seen[e] = struct{}{}
			unique = append(unique, e)
		}
		nr, _, nodeIDs, dErr := s.disableClientsByEmails(tx, unique, nowMs)
		if dErr != nil {
			logger.Warning("enforceFairUsage disable:", dErr)
		}
		if nr {
			needRestart = true
		}
		if len(unique) > 0 {
			clientsDisabled = true
		}
		disabledNodeIDs = append(disabledNodeIDs, nodeIDs...)
	}

	return needRestart, clientsDisabled, uniqueInts(disabledNodeIDs), nil
}

func publishFupExceeded(email, period string, used, limit int64, action string) {
	if eventBus == nil {
		return
	}
	eventBus.Publish(eventbus.Event{
		Type:   eventbus.EventFUPExceeded,
		Source: email,
		Data: &eventbus.FUPExceededData{
			Period: period,
			Used:   used,
			Limit:  limit,
			Action: action,
		},
		Timestamp: time.Now(),
	})
}

func (s *InboundService) disableClientsByEmails(tx *gorm.DB, emails []string, nowMs int64) (bool, int64, []int, error) {
	if len(emails) == 0 {
		return false, 0, nil, nil
	}
	needRestart := false

	type target struct {
		InboundID int  `gorm:"column:inbound_id"`
		NodeID    *int `gorm:"column:node_id"`
		Tag       string
		Email     string
	}
	var targets []target
	err := tx.Raw(`
		SELECT inbounds.id AS inbound_id, inbounds.node_id AS node_id,
		       inbounds.tag AS tag, clients.email AS email
		FROM clients
		JOIN client_inbounds ON client_inbounds.client_id = clients.id
		JOIN inbounds        ON inbounds.id = client_inbounds.inbound_id
		JOIN client_traffics ON client_traffics.email = clients.email
		WHERE clients.email IN ? AND client_traffics.enable = true
	`, emails).Scan(&targets).Error
	if err != nil {
		return false, 0, nil, err
	}

	var localTargets []target
	localByInbound := make(map[int]map[string]struct{})
	remoteByInbound := make(map[int][]target)
	for _, t := range targets {
		if t.NodeID == nil {
			localTargets = append(localTargets, t)
			if localByInbound[t.InboundID] == nil {
				localByInbound[t.InboundID] = make(map[string]struct{})
			}
			localByInbound[t.InboundID][t.Email] = struct{}{}
		} else {
			remoteByInbound[t.InboundID] = append(remoteByInbound[t.InboundID], t)
		}
	}

	if p != nil && len(localTargets) > 0 {
		s.xrayApi.Init(p.GetAPIPort())
		for _, t := range localTargets {
			if err1 := s.xrayApi.RemoveUser(t.Tag, t.Email); err1 != nil {
				needRestart = true
			}
		}
		s.xrayApi.Close()
	}

	for inboundID, emailSet := range localByInbound {
		if _, _, mErr := s.markClientsDisabledInSettings(tx, inboundID, emailSet); mErr != nil {
			logger.Warning("disableClientsByEmails settings sync:", mErr)
		}
	}

	result := tx.Model(xray.ClientTraffic{}).
		Where("email IN ? AND enable = ?", emails, true).
		Update("enable", false)
	if result.Error != nil {
		return needRestart, 0, nil, result.Error
	}

	if err := tx.Model(&model.ClientRecord{}).
		Where("email IN ?", emails).
		Updates(map[string]any{"enable": false, "updated_at": nowMs}).Error; err != nil {
		logger.Warning("disableClientsByEmails update clients.enable:", err)
	}

	disabledNodeIDs := make(map[int]struct{})
	for inboundID, group := range remoteByInbound {
		emailsMap := make(map[string]struct{}, len(group))
		for _, t := range group {
			emailsMap[t.Email] = struct{}{}
		}
		if pushErr := s.disableRemoteClients(tx, inboundID, emailsMap); pushErr != nil {
			needRestart = true
		} else {
			for _, t := range group {
				if t.NodeID != nil {
					disabledNodeIDs[*t.NodeID] = struct{}{}
				}
			}
		}
	}

	nodeIDs := make([]int, 0, len(disabledNodeIDs))
	for id := range disabledNodeIDs {
		nodeIDs = append(nodeIDs, id)
	}
	return needRestart, result.RowsAffected, nodeIDs, nil
}

func (s *InboundService) enableClientsByEmails(tx *gorm.DB, emails []string, nowMs int64) (bool, []int, error) {
	if len(emails) == 0 {
		return false, nil, nil
	}

	var traffics []xray.ClientTraffic
	if err := tx.Where("email IN ? AND enable = ?", emails, false).Find(&traffics).Error; err != nil {
		return false, nil, err
	}
	if len(traffics) == 0 {
		return false, nil, nil
	}

	// Skip re-enable when quota/expiry still blocks the client.
	enabledEmails := make([]string, 0, len(traffics))
	for i := range traffics {
		if isClientQuotaDepleted(&traffics[i], nowMs) {
			continue
		}
		enabledEmails = append(enabledEmails, traffics[i].Email)
	}
	if len(enabledEmails) == 0 {
		return false, nil, nil
	}

	type target struct {
		InboundID int
		NodeID    *int
		Tag       string
		Protocol  string
		Email     string
	}
	var targets []target
	err := tx.Raw(`
		SELECT inbounds.id AS inbound_id, inbounds.node_id AS node_id,
		       inbounds.tag AS tag, inbounds.protocol AS protocol, clients.email AS email
		FROM clients
		JOIN client_inbounds ON client_inbounds.client_id = clients.id
		JOIN inbounds        ON inbounds.id = client_inbounds.inbound_id
		WHERE clients.email IN ?
	`, enabledEmails).Scan(&targets).Error
	if err != nil {
		return false, nil, err
	}

	needRestart := false
	var clientsToAdd []struct {
		protocol string
		tag      string
		client   map[string]any
	}

	inboundIDs := make([]int, 0)
	seenInbound := make(map[int]struct{})
	for _, t := range targets {
		if _, ok := seenInbound[t.InboundID]; !ok {
			seenInbound[t.InboundID] = struct{}{}
			inboundIDs = append(inboundIDs, t.InboundID)
		}
	}

	var inbounds []*model.Inbound
	for _, batch := range chunkInts(inboundIDs, sqliteMaxVars) {
		var page []*model.Inbound
		if err := tx.Where("id IN ?", batch).Find(&page).Error; err != nil {
			return false, nil, err
		}
		inbounds = append(inbounds, page...)
	}

	emailSet := make(map[string]struct{}, len(enabledEmails))
	for _, e := range enabledEmails {
		emailSet[e] = struct{}{}
	}

	for _, ib := range inbounds {
		settings := map[string]any{}
		if err := json.Unmarshal([]byte(ib.Settings), &settings); err != nil {
			continue
		}
		clients, ok := settings["clients"].([]any)
		if !ok {
			continue
		}
		mutated := false
		for i := range clients {
			entry, ok := clients[i].(map[string]any)
			if !ok {
				continue
			}
			email, _ := entry["email"].(string)
			if _, hit := emailSet[email]; !hit {
				continue
			}
			if cur, _ := entry["enable"].(bool); cur {
				continue
			}
			entry["enable"] = true
			entry["updated_at"] = nowMs
			clients[i] = entry
			mutated = true
			if ib.NodeID == nil {
				clientsToAdd = append(clientsToAdd, struct {
					protocol string
					tag      string
					client   map[string]any
				}{protocol: string(ib.Protocol), tag: ib.Tag, client: entry})
			}
		}
		if !mutated {
			continue
		}
		settings["clients"] = clients
		bs, mErr := json.MarshalIndent(settings, "", "  ")
		if mErr != nil {
			continue
		}
		ib.Settings = string(bs)
		if err := tx.Model(&model.Inbound{}).Where("id = ?", ib.Id).Update("settings", ib.Settings).Error; err != nil {
			return false, nil, err
		}
		cs, gcErr := s.GetClients(ib)
		if gcErr == nil {
			_ = s.clientService.SyncInbound(tx, ib.Id, cs)
		}
	}

	if err := tx.Model(xray.ClientTraffic{}).
		Where("email IN ?", enabledEmails).
		Update("enable", true).Error; err != nil {
		return false, nil, err
	}
	if err := tx.Model(&model.ClientRecord{}).
		Where("email IN ?", enabledEmails).
		Updates(map[string]any{"enable": true, "updated_at": nowMs}).Error; err != nil {
		logger.Warning("enableClientsByEmails update clients.enable:", err)
	}

	if p != nil && len(clientsToAdd) > 0 {
		if err1 := s.xrayApi.Init(p.GetAPIPort()); err1 == nil {
			for _, c := range clientsToAdd {
				if err1 := s.xrayApi.AddUser(c.protocol, c.tag, c.client); err1 != nil {
					needRestart = true
				}
			}
			s.xrayApi.Close()
		} else {
			needRestart = true
		}
	}

	return needRestart, nil, nil
}

func resetFupBaselines(tx *gorm.DB, emails []string) error {
	if len(emails) == 0 {
		return nil
	}
	for _, batch := range chunkStrings(emails, sqlInChunk) {
		var stats []xray.ClientTraffic
		if err := tx.Where("email IN ?", batch).Find(&stats).Error; err != nil {
			return err
		}
		overlayGlobalTrafficValues(tx, stats)
		usageByEmail := make(map[string]int64, len(stats))
		for i := range stats {
			usageByEmail[stats[i].Email] = clientTotalUsage(&stats[i])
		}
		now := time.Now()
		for _, email := range batch {
			usage := usageByEmail[email]
			var rec model.ClientRecord
			if err := tx.Where("email = ?", email).First(&rec).Error; err != nil {
				continue
			}
			state := model.ClientFupState{Email: email}
			_ = tx.Where("email = ?", email).FirstOrInit(&state)
			resetTime := rec.FupResetTime
			if rec.FupDailyLimitGB > 0 {
				state.DailyBaseline = usage
				state.DailyPeriodStart = fupPeriodStart(now, resetTime, model.FupPeriodDaily, 0, 0).UnixMilli()
				state.DailyNotified = false
			}
			if rec.FupWeeklyLimitGB > 0 {
				state.WeeklyBaseline = usage
				state.WeeklyPeriodStart = fupPeriodStart(now, resetTime, model.FupPeriodWeekly, rec.FupWeeklyResetDay, 0).UnixMilli()
				state.WeeklyNotified = false
			}
			if rec.FupMonthlyLimitGB > 0 {
				state.MonthlyBaseline = usage
				state.MonthlyPeriodStart = fupPeriodStart(now, resetTime, model.FupPeriodMonthly, 0, rec.FupMonthlyResetDay).UnixMilli()
				state.MonthlyNotified = false
			}
			state.FupDisabledByFup = false
			state.FupDisabledUntil = 0
			state.FupTriggerPeriod = ""
			if err := tx.Save(&state).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func loadFupStatesByEmail(db *gorm.DB, emails []string) map[string]*model.ClientFupState {
	out := make(map[string]*model.ClientFupState)
	if len(emails) == 0 {
		return out
	}
	for _, batch := range chunkStrings(emails, sqlInChunk) {
		var rows []model.ClientFupState
		if err := db.Where("email IN ?", batch).Find(&rows).Error; err != nil {
			continue
		}
		for i := range rows {
			out[rows[i].Email] = &rows[i]
		}
	}
	return out
}
