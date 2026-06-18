package job

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/gary/dune/internal/database"
	"github.com/gary/dune/internal/database/model"
	"github.com/gary/dune/internal/logger"
	"github.com/gary/dune/internal/web/service"
	"github.com/gary/dune/internal/xray"

	"gorm.io/gorm"
)

// IPWithTimestamp tracks an IP address with its last seen timestamp
type IPWithTimestamp struct {
	IP        string `json:"ip"`
	Timestamp int64  `json:"timestamp"`
}

// processedClientScan holds one client's observations from the current IP scan.
type processedClientScan struct {
	lookup      *clientIpLookup
	ipsWithTime []IPWithTimestamp
}

// clientIpLookup bundles the inbound row with per-client limit metadata from
// the normalized clients table. The IP-limit job used to json.Unmarshal the
// full inbound.Settings blob on every scan tick per online user to read a
// single limitIp field — that was the dominant CPU cost on busy panels.
type clientIpLookup struct {
	Inbound               *model.Inbound
	LimitIP               int
	ClientEnable          bool
	HasEnabledInbound     bool // true when the client is linked to at least one enabled inbound
	HasPoolLimitedInbound bool // true when linked to an enabled inbound with inbound.limit_ip > 0
}

// needsLimitWork reports whether this client participates in IP-limit tracking
// this scan. Collection-only runs (enforce=false) always track; enforcement
// runs skip clients with no per-client limit and no pool-limited inbound.
func (l *clientIpLookup) needsLimitWork(enforce bool) bool {
	if !enforce {
		return true
	}
	if l.LimitIP > 0 && l.ClientEnable && l.HasEnabledInbound {
		return true
	}
	return l.HasPoolLimitedInbound
}

// needsPerClientEnforcement is true when the per-client limit path must run.
func (l *clientIpLookup) needsPerClientEnforcement(enforce bool) bool {
	if !enforce {
		return true
	}
	return l.LimitIP > 0 && l.ClientEnable && l.HasEnabledInbound
}

// CheckClientIpJob monitors client IP addresses and manages IP blocking based
// on configured limits. The per-client IPs come from the core's online-stats
// API when the running core supports it (no access log needed), falling back
// to access-log parsing on older cores.
type CheckClientIpJob struct {
	lastClear     int64
	disAllowedIps []string
	xrayService   service.XrayService
}

var job *CheckClientIpJob

const defaultXrayAPIPort = 62789

const ipStaleAfterSeconds = int64(30 * 60)

// NewCheckClientIpJob creates a new client IP monitoring job instance.
func NewCheckClientIpJob() *CheckClientIpJob {
	job = new(CheckClientIpJob)
	return job
}

func (j *CheckClientIpJob) Run() {
	if j.lastClear == 0 {
		j.lastClear = time.Now().Unix()
	}

	fail2BanEnabled := isFail2BanEnabled()
	hasLimit := fail2BanEnabled && j.hasLimitIp()
	f2bInstalled := false
	if hasLimit {
		f2bInstalled = j.checkFail2BanInstalled()
	}

	if hasLimit {
		if observed, apiMode := j.collectFromOnlineAPI(); apiMode {
			if len(observed) > 0 {
				j.processObserved(observed, j.resolveEnforce(true, f2bInstalled), true)
			}
			// The core tracks online IPs itself, so no access log is needed in this
			// mode; still rotate a user-configured access log hourly so it doesn't
			// grow unboundedly. The enforcement-triggered rotation is skipped —
			// nothing here reads the log.
			if j.checkAccessLogAvailable(false) && time.Now().Unix()-j.lastClear > 3600 {
				j.clearAccessLog()
			}
			return
		}
		// Online-stats unavailable: fall through to access-log parsing below.
	}

	shouldClearAccessLog := false
	isAccessLogAvailable := j.checkAccessLogAvailable(hasLimit)

	if fail2BanEnabled && isAccessLogAvailable {
		shouldClearAccessLog = j.processLogFile(j.resolveEnforce(hasLimit, f2bInstalled))
	}

	if shouldClearAccessLog || (isAccessLogAvailable && time.Now().Unix()-j.lastClear > 3600) {
		j.clearAccessLog()
	}
}

// resolveEnforce decides whether limits can actually be enforced this run,
// warning when fail2ban is missing on a platform that needs it.
func (j *CheckClientIpJob) resolveEnforce(hasLimit, f2bInstalled bool) bool {
	if hasLimit && runtime.GOOS != "windows" && !f2bInstalled {
		logger.Warning("[LimitIP] Fail2Ban is not installed, Please install Fail2Ban from the dune bash menu.")
		return false
	}
	return hasLimit
}

// collectFromOnlineAPI builds per-email IP observations (email -> ip ->
// last-seen unix seconds) from the core's online-stats API. ok=false means the
// API is unavailable — xray not running, an older core, or a transient gRPC
// failure — and the caller must fall back to access-log parsing.
func (j *CheckClientIpJob) collectFromOnlineAPI() (map[string]map[string]int64, bool) {
	var lastErr error
	for attempt := 0; attempt < 2; attempt++ {
		onlineUsers, ok, err := j.xrayService.GetOnlineUsers()
		if ok {
			now := time.Now().Unix()
			observed := make(map[string]map[string]int64, len(onlineUsers))
			for _, user := range onlineUsers {
				for _, entry := range user.IPs {
					// No localhost guard needed here: the core's OnlineMap.AddIP drops
					// 127.0.0.1/[::1] itself, so they never reach this list.
					ts := entry.LastSeen
					if ts <= 0 {
						ts = now
					}
					if _, exists := observed[user.Email]; !exists {
						observed[user.Email] = make(map[string]int64)
					}
					if existing, seen := observed[user.Email][entry.IP]; !seen || ts > existing {
						observed[user.Email][entry.IP] = ts
					}
				}
			}
			return observed, true
		}
		lastErr = err
		if attempt == 0 && j.xrayService.OnlineStatsKnownSupported() {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		break
	}
	if lastErr != nil {
		if j.xrayService.OnlineStatsKnownSupported() {
			logger.Warning("[LimitIP] online-stats API poll failed (will retry next tick):", lastErr)
		} else {
			logger.Debug("[LimitIP] online-stats API unavailable this run:", lastErr)
		}
	}
	return nil, false
}

func (j *CheckClientIpJob) clearAccessLog() {
	logAccessP, err := os.OpenFile(xray.GetAccessPersistentLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	j.checkError(err)
	defer logAccessP.Close()

	accessLogPath, err := xray.GetAccessLogPath()
	j.checkError(err)

	file, err := os.Open(accessLogPath)
	j.checkError(err)
	defer file.Close()

	_, err = io.Copy(logAccessP, file)
	j.checkError(err)

	err = os.Truncate(accessLogPath, 0)
	j.checkError(err)

	j.lastClear = time.Now().Unix()
}

func (j *CheckClientIpJob) hasLimitIp() bool {
	db := database.GetDB()
	var found int64
	if err := db.Model(&model.ClientRecord{}).Where("limit_ip > 0").Limit(1).Count(&found).Error; err == nil && found > 0 {
		return true
	}
	err := db.Model(&model.Inbound{}).Where("limit_ip > 0").Limit(1).Count(&found).Error
	return err == nil && found > 0
}

func (j *CheckClientIpJob) processLogFile(enforce bool) bool {

	ipRegex := regexp.MustCompile(`from (?:tcp:|udp:)?\[?([0-9a-fA-F\.:]+)\]?:\d+ accepted`)
	emailRegex := regexp.MustCompile(`email: (.+)$`)
	timestampRegex := regexp.MustCompile(`^(\d{4}/\d{2}/\d{2} \d{2}:\d{2}:\d{2})`)

	accessLogPath, _ := xray.GetAccessLogPath()
	file, _ := os.Open(accessLogPath)
	defer file.Close()

	// Track IPs with their last seen timestamp
	inboundClientIps := make(map[string]map[string]int64, 100)

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := scanner.Text()

		ipMatches := ipRegex.FindStringSubmatch(line)
		if len(ipMatches) < 2 {
			continue
		}

		ip := ipMatches[1]

		if ip == "127.0.0.1" || ip == "::1" {
			continue
		}

		emailMatches := emailRegex.FindStringSubmatch(line)
		if len(emailMatches) < 2 {
			continue
		}
		email := strings.TrimSpace(emailMatches[1])

		// Extract timestamp from log line
		var timestamp int64
		timestampMatches := timestampRegex.FindStringSubmatch(line)
		if len(timestampMatches) >= 2 {
			t, err := time.ParseInLocation("2006/01/02 15:04:05", timestampMatches[1], time.Local)
			if err == nil {
				timestamp = t.Unix()
			} else {
				timestamp = time.Now().Unix()
			}
		} else {
			timestamp = time.Now().Unix()
		}

		if _, exists := inboundClientIps[email]; !exists {
			inboundClientIps[email] = make(map[string]int64)
		}
		// Update timestamp - keep the latest
		if existingTime, ok := inboundClientIps[email][ip]; !ok || timestamp > existingTime {
			inboundClientIps[email][ip] = timestamp
		}
	}
	if err := scanner.Err(); err != nil {
		j.checkError(err)
	}

	return j.processObserved(inboundClientIps, enforce, false)
}

// processObserved runs collection + enforcement for one scan's observations
// (email -> ip -> last-seen unix seconds). observedAreLive marks the
// observations as live connections (online-stats API) rather than recent log
// lines: live entries bypass the stale cutoff, since a connection that opened
// hours ago is still live even though its timestamp is old.
func (j *CheckClientIpJob) processObserved(observed map[string]map[string]int64, enforce, observedAreLive bool) bool {
	shouldCleanLog := false
	now := time.Now().Unix()
	// Parsed once per inbound per scan, only when a ban triggers disconnect.
	settingsCache := make(map[int][]model.Client)
	processed := make(map[string]processedClientScan, len(observed))
	// attribution accumulates this scan's local observations per email so they can
	// be recorded under this panel's own guid for cross-node IP attribution.
	attribution := make(map[string][]model.ClientIpEntry, len(observed))
	for email, ipTimestamps := range observed {

		// The observations can still reference a client that was just renamed
		// or deleted; its email no longer matches any inbound. Skip it (and
		// drop any orphaned tracking row) instead of recreating a row and
		// logging an ERROR every run (#4963).
		lookup, err := j.getClientIpLookup(email)
		if err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				logger.Debugf("[LimitIP] skipping stale observed email %q (renamed or deleted)", email)
				j.delInboundClientIps(email)
			} else {
				j.checkError(err)
			}
			continue
		}
		if !lookup.needsLimitWork(enforce) {
			continue
		}

		// Convert to IPWithTimestamp slice
		ipsWithTime := make([]IPWithTimestamp, 0, len(ipTimestamps))
		attrEntries := make([]model.ClientIpEntry, 0, len(ipTimestamps))
		for ip, timestamp := range ipTimestamps {
			ipsWithTime = append(ipsWithTime, IPWithTimestamp{IP: ip, Timestamp: timestamp})
			// Live API observations may carry an old lastSeen (connection start),
			// so stamp attribution with now; otherwise the stale cutoff would evict
			// an IP that is connected right now.
			attrTs := timestamp
			if observedAreLive {
				attrTs = now
			}
			attrEntries = append(attrEntries, model.ClientIpEntry{IP: ip, Timestamp: attrTs})
		}
		if len(attrEntries) > 0 {
			attribution[email] = attrEntries
		}
		processed[email] = processedClientScan{lookup: lookup, ipsWithTime: ipsWithTime}

		if !lookup.needsPerClientEnforcement(enforce) {
			continue
		}

		clientIpsRecord, err := j.getInboundClientIps(email)
		if err != nil {
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				j.checkError(err)
				continue
			}
			// First sighting: enforce on this same tick instead of waiting for
			// the next scan (which could miss a briefly-connected extra IP).
			clientIpsRecord = &model.InboundClientIps{ClientEmail: email}
		}

		shouldCleanLog = j.updateInboundClientIps(clientIpsRecord, lookup, email, ipsWithTime, enforce, observedAreLive, settingsCache) || shouldCleanLog
	}

	shouldCleanLog = j.enforceInboundLimits(processed, enforce, observedAreLive, settingsCache) || shouldCleanLog

	j.recordLocalAttribution(attribution)

	return shouldCleanLog
}

// recordLocalAttribution stores this scan's local observations under this panel's
// own guid so a parent panel can attribute each IP to the node it is on.
// Best-effort: attribution is advisory and must never block IP-limit enforcement.
func (j *CheckClientIpJob) recordLocalAttribution(attribution map[string][]model.ClientIpEntry) {
	if len(attribution) == 0 {
		return
	}
	guid, err := (&service.SettingService{}).GetPanelGuid()
	if err != nil || guid == "" {
		return
	}
	if err := (&service.InboundService{}).RecordLocalClientIps(guid, attribution); err != nil {
		logger.Debug("[LimitIP] record local ip attribution failed:", err)
	}
}

// mergeClientIps folds this scan's observations into the persisted set,
// dropping entries older than staleCutoff. newAlwaysLive exempts the new
// entries from that cutoff: an API-observed IP is a live connection by
// definition, even when its lastSeen (set at dispatch time) is hours old.
func mergeClientIps(old, new []IPWithTimestamp, staleCutoff int64, newAlwaysLive bool) map[string]int64 {
	ipMap := make(map[string]int64, len(old)+len(new))
	for _, ipTime := range old {
		if ipTime.Timestamp < staleCutoff {
			continue
		}
		ipMap[ipTime.IP] = ipTime.Timestamp
	}
	for _, ipTime := range new {
		if !newAlwaysLive && ipTime.Timestamp < staleCutoff {
			continue
		}
		if existingTime, ok := ipMap[ipTime.IP]; !ok || ipTime.Timestamp > existingTime {
			ipMap[ipTime.IP] = ipTime.Timestamp
		}
	}
	return ipMap
}

// selectIpsToBan splits the live IPs (sorted oldest-first by partitionLiveIps)
// into the newest `limit` entries to keep and the older remainder to ban.
func selectIpsToBan(live []IPWithTimestamp, limit int) (kept, banned []IPWithTimestamp) {
	if limit <= 0 || len(live) <= limit {
		return live, nil
	}
	cutoff := len(live) - limit
	return live[cutoff:], live[:cutoff]
}

func partitionLiveIps(ipMap map[string]int64, observedThisScan map[string]bool) (live, historical []IPWithTimestamp) {
	live = make([]IPWithTimestamp, 0, len(observedThisScan))
	historical = make([]IPWithTimestamp, 0, len(ipMap))
	now := time.Now().Unix()
	for ip, ts := range ipMap {
		entry := IPWithTimestamp{IP: ip, Timestamp: ts}
		// Consider an IP "live" if it was seen locally in this scan, OR if its
		// timestamp from the synced database is very recent (e.g. within 2 minutes).
		// This ensures cluster-wide limits work even if the IP was seen on another node.
		if observedThisScan[ip] || now-ts < 120 {
			live = append(live, entry)
		} else {
			historical = append(historical, entry)
		}
	}
	sort.Slice(live, func(i, j int) bool { return live[i].Timestamp < live[j].Timestamp })
	sort.Slice(historical, func(i, j int) bool { return historical[i].Timestamp < historical[j].Timestamp })
	return live, historical
}

func (j *CheckClientIpJob) checkFail2BanInstalled() bool {
	if !isFail2BanEnabled() {
		return false
	}

	cmd := "fail2ban-client"
	args := []string{"-h"}
	err := exec.Command(cmd, args...).Run()
	return err == nil
}

func isFail2BanEnabled() bool {
	value, ok := os.LookupEnv("DUNE_ENABLE_FAIL2BAN")
	return !ok || value == "true"
}

func (j *CheckClientIpJob) checkAccessLogAvailable(iplimitActive bool) bool {
	accessLogPath, err := xray.GetAccessLogPath()
	if err != nil {
		return false
	}

	if accessLogPath == "none" || accessLogPath == "" {
		if iplimitActive {
			logger.Warning("[LimitIP] Access log path is not set, Please configure the access log path in Xray configs.")
		}
		return false
	}

	return true
}

func (j *CheckClientIpJob) checkError(e error) {
	if e != nil {
		logger.Warning("client ip job err:", e)
	}
}

func (j *CheckClientIpJob) getInboundClientIps(clientEmail string) (*model.InboundClientIps, error) {
	db := database.GetDB()
	InboundClientIps := &model.InboundClientIps{}
	err := db.Model(model.InboundClientIps{}).Where("client_email = ?", clientEmail).First(InboundClientIps).Error
	if err != nil {
		return nil, err
	}
	return InboundClientIps, nil
}

func (j *CheckClientIpJob) addInboundClientIps(clientEmail string, ipsWithTime []IPWithTimestamp) error {
	inboundClientIps := &model.InboundClientIps{}
	jsonIps, err := json.Marshal(ipsWithTime)
	j.checkError(err)

	inboundClientIps.ClientEmail = clientEmail
	inboundClientIps.Ips = string(jsonIps)

	db := database.GetDB()
	tx := db.Begin()

	defer func() {
		if err == nil {
			tx.Commit()
		} else {
			tx.Rollback()
		}
	}()

	err = tx.Save(inboundClientIps).Error
	if err != nil {
		return err
	}
	return nil
}

// delInboundClientIps drops the inbound_client_ips tracking row for an email
// that no longer maps to any inbound (a renamed or deleted client), so stale
// access-log entries don't keep a ghost row alive (#4963).
func (j *CheckClientIpJob) delInboundClientIps(clientEmail string) {
	db := database.GetDB()
	if err := db.Where("client_email = ?", clientEmail).Delete(&model.InboundClientIps{}).Error; err != nil {
		j.checkError(err)
	}
}

func ipMapToSlice(ipMap map[string]int64) []IPWithTimestamp {
	out := make([]IPWithTimestamp, 0, len(ipMap))
	for ip, ts := range ipMap {
		out = append(out, IPWithTimestamp{IP: ip, Timestamp: ts})
	}
	return out
}

func (j *CheckClientIpJob) persistObservedIPs(inboundClientIps *model.InboundClientIps, newIps []IPWithTimestamp, observedAreLive bool) {
	var oldIps []IPWithTimestamp
	if inboundClientIps.Ips != "" {
		json.Unmarshal([]byte(inboundClientIps.Ips), &oldIps)
	}
	merged := mergeClientIps(oldIps, newIps, time.Now().Unix()-ipStaleAfterSeconds, observedAreLive)
	ips := ipMapToSlice(merged)

	jsonIps, err := json.Marshal(ips)
	if err != nil {
		j.checkError(err)
		return
	}
	inboundClientIps.Ips = string(jsonIps)
	if err := database.GetDB().Save(inboundClientIps).Error; err != nil {
		j.checkError(err)
	}
}

func (j *CheckClientIpJob) updateInboundClientIps(inboundClientIps *model.InboundClientIps, lookup *clientIpLookup, clientEmail string, newIpsWithTime []IPWithTimestamp, enforce, observedAreLive bool, settingsCache map[int][]model.Client) bool {
	inbound := lookup.Inbound
	limitIp := lookup.LimitIP

	if !enforce || limitIp <= 0 || !lookup.ClientEnable || !lookup.HasEnabledInbound {
		// Nothing to enforce (collection-only run, no limit, client disabled, or
		// no enabled inbound): record the observed IPs for the panel and return.
		j.persistObservedIPs(inboundClientIps, newIpsWithTime, observedAreLive)
		return false
	}

	// Parse old IPs from database
	var oldIpsWithTime []IPWithTimestamp
	if inboundClientIps.Ips != "" {
		json.Unmarshal([]byte(inboundClientIps.Ips), &oldIpsWithTime)
	}

	ipMap := mergeClientIps(oldIpsWithTime, newIpsWithTime, time.Now().Unix()-ipStaleAfterSeconds, observedAreLive)

	// only ips seen in this scan count toward the limit. see
	// partitionLiveIps.
	observedThisScan := make(map[string]bool, len(newIpsWithTime))
	for _, ipTime := range newIpsWithTime {
		observedThisScan[ipTime.IP] = true
	}
	liveIps, historicalIps := partitionLiveIps(ipMap, observedThisScan)

	shouldCleanLog := false
	j.disAllowedIps = []string{}

	// historical db-only ips are excluded from this count on purpose.
	keptLive, bannedLive := selectIpsToBan(liveIps, limitIp)
	if len(bannedLive) > 0 {
		shouldCleanLog = true

		logIpFile, err := os.OpenFile(xray.GetIPLimitLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			logger.Errorf("failed to open IP limit log file: %s", err)
			return false
		}
		defer logIpFile.Close()
		ipLogger := log.New(logIpFile, "", log.LstdFlags)

		// log format is load-bearing: dune.sh create_iplimit_jails builds
		// filter.d/dune-ipl.conf with
		//   failregex = \[LIMIT_IP\]\s*Email\s*=\s*<F-USER>.+</F-USER>\s*\|\|\s*Disconnecting OLD IP\s*=\s*<ADDR>\s*\|\|\s*Timestamp\s*=\s*\d+
		// don't change the wording.
		for _, ipTime := range bannedLive {
			j.disAllowedIps = append(j.disAllowedIps, ipTime.IP)
			ipLogger.Printf("[LIMIT_IP] Email = %s || Disconnecting OLD IP = %s || Timestamp = %d", clientEmail, ipTime.IP, ipTime.Timestamp)
		}

		// force xray to drop existing connections from banned ips
		if clients, err := j.inboundClientsCached(inbound, settingsCache); err != nil {
			logger.Warningf("[LIMIT_IP] could not load inbound clients for disconnect: %v", err)
		} else {
			j.disconnectClientTemporarily(inbound, clientEmail, clients)
		}
	}

	// keep kept-live + historical in the blob so the panel keeps showing
	// recently seen ips. banned live ips are already in the fail2ban log
	// and will reappear in the next scan if they reconnect.
	dbIps := make([]IPWithTimestamp, 0, len(keptLive)+len(historicalIps))
	dbIps = append(dbIps, keptLive...)
	dbIps = append(dbIps, historicalIps...)
	jsonIps, _ := json.Marshal(dbIps)
	inboundClientIps.Ips = string(jsonIps)

	db := database.GetDB()
	err := db.Save(inboundClientIps).Error
	if err != nil {
		logger.Error("failed to save inboundClientIps:", err)
		return false
	}

	if len(j.disAllowedIps) > 0 {
		logger.Infof("[LIMIT_IP] Client %s: Kept %d live IPs, queued %d old IPs for fail2ban", clientEmail, len(keptLive), len(j.disAllowedIps))
	}

	return shouldCleanLog
}

// disconnectClientTemporarily removes and re-adds a client to force disconnect banned connections
func (j *CheckClientIpJob) disconnectClientTemporarily(inbound *model.Inbound, clientEmail string, clients []model.Client) {
	var xrayAPI xray.XrayAPI
	apiPort := j.resolveXrayAPIPort()

	err := xrayAPI.Init(apiPort)
	if err != nil {
		logger.Warningf("[LIMIT_IP] Failed to init Xray API for disconnection: %v", err)
		return
	}
	defer xrayAPI.Close()

	// Find the client config
	var clientConfig map[string]any
	for _, client := range clients {
		if client.Email == clientEmail {
			// Convert client to map for API
			clientBytes, _ := json.Marshal(client)
			json.Unmarshal(clientBytes, &clientConfig)
			break
		}
	}

	if clientConfig == nil {
		return
	}

	// Only perform remove/re-add for protocols supported by XrayAPI.AddUser
	protocol := string(inbound.Protocol)
	switch protocol {
	case "vmess", "vless", "trojan", "shadowsocks":
		// supported protocols, continue
	default:
		logger.Warningf("[LIMIT_IP] Temporary disconnect is not supported for protocol %s on inbound %s", protocol, inbound.Tag)
		return
	}

	// For Shadowsocks, ensure the required "cipher" field is present by
	// reading it from the inbound settings (e.g., settings["method"]).
	if string(inbound.Protocol) == "shadowsocks" {
		var inboundSettings map[string]any
		if err := json.Unmarshal([]byte(inbound.Settings), &inboundSettings); err != nil {
			logger.Warningf("[LIMIT_IP] Failed to parse inbound settings for shadowsocks cipher: %v", err)
		} else {
			if method, ok := inboundSettings["method"].(string); ok && method != "" {
				clientConfig["cipher"] = method
			}
		}
	}

	// Remove user to disconnect all connections
	err = xrayAPI.RemoveUser(inbound.Tag, clientEmail)
	if err != nil {
		logger.Warningf("[LIMIT_IP] Failed to remove user %s: %v", clientEmail, err)
		return
	}

	// Wait a moment for disconnection to take effect
	time.Sleep(100 * time.Millisecond)

	// Re-add user to allow new connections
	err = xrayAPI.AddUser(protocol, inbound.Tag, clientConfig)
	if err != nil {
		logger.Warningf("[LIMIT_IP] Failed to re-add user %s: %v", clientEmail, err)
	}
}

// resolveXrayAPIPort returns the API inbound port from running config, then template config, then default.
func (j *CheckClientIpJob) resolveXrayAPIPort() int {
	var configErr error
	var templateErr error

	if port, err := getAPIPortFromConfigPath(xray.GetConfigPath()); err == nil {
		return port
	} else {
		configErr = err
	}

	db := database.GetDB()
	var template model.Setting
	if err := db.Where("key = ?", "xrayTemplateConfig").First(&template).Error; err == nil {
		if port, parseErr := getAPIPortFromConfigData([]byte(template.Value)); parseErr == nil {
			return port
		} else {
			templateErr = parseErr
		}
	} else {
		templateErr = err
	}

	logger.Warningf(
		"[LIMIT_IP] Could not determine Xray API port from config or template; falling back to default port %d (config error: %v, template error: %v)",
		defaultXrayAPIPort,
		configErr,
		templateErr,
	)

	return defaultXrayAPIPort
}

func getAPIPortFromConfigPath(configPath string) (int, error) {
	configData, err := os.ReadFile(configPath)
	if err != nil {
		return 0, err
	}

	return getAPIPortFromConfigData(configData)
}

func getAPIPortFromConfigData(configData []byte) (int, error) {
	xrayConfig := &xray.Config{}
	if err := json.Unmarshal(configData, xrayConfig); err != nil {
		return 0, err
	}

	for _, inboundConfig := range xrayConfig.InboundConfigs {
		if inboundConfig.Tag == "api" && inboundConfig.Port > 0 {
			return inboundConfig.Port, nil
		}
	}

	return 0, errors.New("api inbound port not found")
}

func (j *CheckClientIpJob) listInboundClientEmails(inboundID int) ([]string, error) {
	var emails []string
	err := database.GetDB().Table("clients").
		Select("clients.email").
		Joins("JOIN client_inbounds ON client_inbounds.client_id = clients.id").
		Where("client_inbounds.inbound_id = ?", inboundID).
		Pluck("clients.email", &emails).Error
	return emails, err
}

type inboundLiveIP struct {
	IP    string
	TS    int64
	Email string
}

func (j *CheckClientIpJob) liveIPsForClient(email string, scan processedClientScan, observedAreLive bool) []IPWithTimestamp {
	newIps := scan.ipsWithTime
	observedThisScan := make(map[string]bool, len(newIps))
	for _, ipt := range newIps {
		observedThisScan[ipt.IP] = true
	}
	var oldIps []IPWithTimestamp
	if rec, err := j.getInboundClientIps(email); err == nil && rec.Ips != "" {
		json.Unmarshal([]byte(rec.Ips), &oldIps)
	}
	ipMap := mergeClientIps(oldIps, newIps, time.Now().Unix()-ipStaleAfterSeconds, observedAreLive)
	live, _ := partitionLiveIps(ipMap, observedThisScan)
	return live
}

// enforceInboundLimits applies a cap on unique live source IPs across every
// client attached to an inbound. Per-client limits are enforced earlier in
// updateInboundClientIps; this pass catches shared-pool overuse.
func (j *CheckClientIpJob) enforceInboundLimits(processed map[string]processedClientScan, enforce, observedAreLive bool, settingsCache map[int][]model.Client) bool {
	if !enforce {
		return false
	}

	var limitedInbounds []model.Inbound
	if err := database.GetDB().Where("limit_ip > 0 AND enable = ?", true).Find(&limitedInbounds).Error; err != nil {
		j.checkError(err)
		return false
	}
	if len(limitedInbounds) == 0 {
		return false
	}

	shouldCleanLog := false
	for i := range limitedInbounds {
		inbound := &limitedInbounds[i]
		limit := inbound.LimitIP
		emails, err := j.listInboundClientEmails(inbound.Id)
		if err != nil {
			j.checkError(err)
			continue
		}

		liveByIP := make(map[string]inboundLiveIP)
		for _, email := range emails {
			scan := processed[email]
			for _, entry := range j.liveIPsForClient(email, scan, observedAreLive) {
				if cur, exists := liveByIP[entry.IP]; !exists || entry.Timestamp < cur.TS {
					liveByIP[entry.IP] = inboundLiveIP{IP: entry.IP, TS: entry.Timestamp, Email: email}
				}
			}
		}
		if len(liveByIP) <= limit {
			continue
		}

		liveList := make([]inboundLiveIP, 0, len(liveByIP))
		for _, entry := range liveByIP {
			liveList = append(liveList, entry)
		}
		sort.Slice(liveList, func(i, j int) bool { return liveList[i].TS < liveList[j].TS })
		toBan := liveList[:len(liveList)-limit]
		shouldCleanLog = true

		logIpFile, err := os.OpenFile(xray.GetIPLimitLogPath(), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			logger.Errorf("failed to open IP limit log file: %s", err)
			continue
		}
		ipLogger := log.New(logIpFile, "", log.LstdFlags)
		for _, banned := range toBan {
			j.disAllowedIps = append(j.disAllowedIps, banned.IP)
			ipLogger.Printf("[LIMIT_IP] Email = %s || Disconnecting OLD IP = %s || Timestamp = %d", banned.Email, banned.IP, banned.TS)
			if clients, err := j.inboundClientsCached(inbound, settingsCache); err != nil {
				logger.Warningf("[LIMIT_IP] could not load inbound clients for disconnect: %v", err)
			} else {
				j.disconnectClientTemporarily(inbound, banned.Email, clients)
			}
			j.removeIPFromClientRecord(banned.Email, banned.IP)
		}
		logIpFile.Close()

		logger.Infof("[LIMIT_IP] Inbound %s: kept %d live IPs, queued %d for fail2ban (inbound limit %d)",
			inbound.Tag, limit, len(toBan), limit)
	}
	return shouldCleanLog
}

func (j *CheckClientIpJob) removeIPFromClientRecord(email, bannedIP string) {
	rec, err := j.getInboundClientIps(email)
	if err != nil {
		return
	}
	var ips []IPWithTimestamp
	if rec.Ips != "" {
		json.Unmarshal([]byte(rec.Ips), &ips)
	}
	filtered := make([]IPWithTimestamp, 0, len(ips))
	for _, entry := range ips {
		if entry.IP != bannedIP {
			filtered = append(filtered, entry)
		}
	}
	jsonIps, err := json.Marshal(filtered)
	if err != nil {
		j.checkError(err)
		return
	}
	rec.Ips = string(jsonIps)
	if err := database.GetDB().Save(rec).Error; err != nil {
		j.checkError(err)
	}
}

func (j *CheckClientIpJob) getClientIpLookup(clientEmail string) (*clientIpLookup, error) {
	db := database.GetDB()
	row := struct {
		model.Inbound
		LimitIP               int  `gorm:"column:limit_ip"`
		ClientEnable          bool `gorm:"column:client_enable"`
		HasEnabledInbound     bool `gorm:"column:has_enabled_inbound"`
		HasPoolLimitedInbound bool `gorm:"column:has_pool_limited_inbound"`
	}{}

	err := db.Table("inbounds").
		Select(`inbounds.*, clients.limit_ip, clients.enable AS client_enable,
			EXISTS (
				SELECT 1 FROM client_inbounds ci2
				INNER JOIN inbounds i2 ON i2.id = ci2.inbound_id
				WHERE ci2.client_id = clients.id AND i2.enable = ?
			) AS has_enabled_inbound,
			EXISTS (
				SELECT 1 FROM client_inbounds ci2
				INNER JOIN inbounds i2 ON i2.id = ci2.inbound_id
				WHERE ci2.client_id = clients.id AND i2.enable = ? AND i2.limit_ip > 0
			) AS has_pool_limited_inbound`, true, true).
		Joins("JOIN client_inbounds ON client_inbounds.inbound_id = inbounds.id").
		Joins("JOIN clients ON clients.id = client_inbounds.client_id").
		Where("clients.email = ?", clientEmail).
		Order("inbounds.enable DESC, inbounds.id ASC").
		First(&row).Error
	if err != nil {
		return nil, err
	}

	inbound := row.Inbound
	return &clientIpLookup{
		Inbound:               &inbound,
		LimitIP:               row.LimitIP,
		ClientEnable:          row.ClientEnable,
		HasEnabledInbound:     row.HasEnabledInbound,
		HasPoolLimitedInbound: row.HasPoolLimitedInbound,
	}, nil
}

// inboundClientsCached parses inbound.Settings at most once per inbound per
// scan. Only the rare ban/disconnect path needs the full client list.
func (j *CheckClientIpJob) inboundClientsCached(inbound *model.Inbound, cache map[int][]model.Client) ([]model.Client, error) {
	if inbound == nil {
		return nil, errors.New("missing inbound")
	}
	if cached, ok := cache[inbound.Id]; ok {
		return cached, nil
	}
	if inbound.Settings == "" {
		return nil, errors.New("empty inbound settings")
	}
	settings := map[string][]model.Client{}
	if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
		return nil, err
	}
	clients := settings["clients"]
	cache[inbound.Id] = clients
	return clients, nil
}
