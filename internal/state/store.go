// Package state owns the small, credential-bearing daemon state file.
package state

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"github.com/Taurine-Technology/axon-pulse/internal/protocol"
	"github.com/Taurine-Technology/axon-pulse/quality"
)

type (
	// PrivacyPurgeAction records the strongest destructive cleanup that must
	// finish before uploads may resume. It makes disconnect and revocation
	// recovery unambiguous after a crash or failed state write.
	PrivacyPurgeAction string

	State struct {
		SensorUID            string                `json:"sensor_uid"`
		Mode                 string                `json:"mode"`
		ControllerURL        string                `json:"controller_url,omitempty"`
		SensorID             string                `json:"sensor_id,omitempty"`
		SensorSecret         string                `json:"sensor_secret,omitempty"`
		SiteID               string                `json:"site_id,omitempty"`
		IngestURL            string                `json:"ingest_url,omitempty"`
		Config               protocol.SensorConfig `json:"config"`
		ConfigVersion        int                   `json:"config_version"`
		SSIDConsent          bool                  `json:"ssid_consent"`
		SSIDPurgePending     bool                  `json:"ssid_purge_pending,omitempty"`
		PrivacyPurgeAction   PrivacyPurgeAction    `json:"privacy_purge_action,omitempty"`
		PausedUntil          time.Time             `json:"paused_until,omitzero"`
		RevokedAt            time.Time             `json:"revoked_at,omitzero"`
		LastSpeedTest        time.Time             `json:"last_speed_test,omitzero"`
		LastSpeedTests       map[string]time.Time  `json:"last_speed_tests,omitzero"`
		BudgetDay            string                `json:"budget_day,omitempty"`
		BudgetBytes          uint64                `json:"budget_bytes,omitempty"`
		BudgetMonth          string                `json:"budget_month,omitempty"`
		MonthlyBudgetBytes   uint64                `json:"monthly_budget_bytes,omitempty"`
		HandledSpeedRequests map[string]int64      `json:"handled_speed_requests,omitempty"`
		NetworkIdentityKey   string                `json:"network_identity_key,omitempty"`
		// HouseholdMix is the device-owned override of the household activity
		// mix. Nil defers to controller configuration, then the default.
		HouseholdMix *quality.HouseholdMix `json:"household_mix,omitempty"`
		// UpdateChannel is the release stream this installation follows.
		// Empty means the build default (main) unless the service
		// environment says otherwise.
		UpdateChannel string `json:"update_channel,omitempty"`
	}

	Store struct {
		mu    sync.RWMutex
		path  string
		state State
		write func(State) (bool, error)
		files stateFileOperations
	}

	stateFile interface {
		Name() string
		Write([]byte) (int, error)
		Sync() error
		Close() error
	}

	stateFileOperations struct {
		createTemp    func(string, string) (stateFile, error)
		chmod         func(string, os.FileMode) error
		remove        func(string) error
		replace       func(string, string) error
		syncDirectory func(string) error
	}
)

const (
	// UpdateChannelMain, UpdateChannelBeta, and UpdateChannelAlpha name the
	// release streams a sensor may follow, most stable first.
	UpdateChannelMain  = "main"
	UpdateChannelBeta  = "beta"
	UpdateChannelAlpha = "alpha"

	StateFilename        = "state.json"
	PrivacyPurgeFilename = "privacy-purge.json"

	ModeSetup      = "setup"
	ModeStandalone = "standalone"
	ModeConnected  = "connected"

	PrivacyPurgeSSID       PrivacyPurgeAction = "ssid"
	PrivacyPurgeDisconnect PrivacyPurgeAction = "disconnect"
	PrivacyPurgeRevocation PrivacyPurgeAction = "revocation"
)

func (s State) Claimed() bool {
	return s.SensorID != "" && s.SensorSecret != "" && s.IngestURL != ""
}

func (s State) Operational() bool {
	return s.Mode == ModeStandalone || (s.Mode == ModeConnected && s.Claimed())
}

func (s State) Paused(now time.Time) bool {
	return s.Config.Paused || s.PausedUntil.After(now)
}

func Open(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("state directory is empty")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create state directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, fmt.Errorf("secure state directory: %w", err)
	}
	store := &Store{
		path: filepath.Join(dir, StateFilename),
		files: stateFileOperations{
			createTemp: func(dir, pattern string) (stateFile, error) {
				return os.CreateTemp(dir, pattern)
			},
			chmod:         os.Chmod,
			remove:        os.Remove,
			replace:       replaceFile,
			syncDirectory: syncDirectory,
		},
	}
	store.write = store.writeState
	data, err := os.ReadFile(store.path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read state: %w", err)
	}
	if err == nil {
		if err := json.Unmarshal(data, &store.state); err != nil {
			return nil, fmt.Errorf("decode state: %w", err)
		}
	}
	if store.state.PrivacyPurgeAction != "" && !validPrivacyPurgeAction(store.state.PrivacyPurgeAction) {
		return nil, fmt.Errorf("decode state: invalid privacy purge action %q", store.state.PrivacyPurgeAction)
	}
	sentinelAction, sentinelPresent, err := store.readPrivacyPurgeSentinel()
	if err != nil {
		return nil, err
	}
	if sentinelPresent {
		beginPrivacyPurge(&store.state, sentinelAction)
	}
	if store.state.SensorUID == "" {
		uid, err := newUUIDv4()
		if err != nil {
			return nil, err
		}
		store.state.SensorUID = uid
	}
	if store.state.Mode == "" {
		if store.state.Claimed() {
			store.state.Mode = ModeConnected
		} else {
			store.state.Mode = ModeSetup
		}
	}
	if store.state.Mode != ModeSetup && store.state.Mode != ModeStandalone && store.state.Mode != ModeConnected {
		store.state.Mode = ModeSetup
	}
	if store.state.Mode == ModeConnected && !store.state.Claimed() {
		store.state.Mode = ModeSetup
	}
	if store.state.PrivacyPurgeAction != "" {
		store.state.SSIDPurgePending = true
	}
	if store.state.SSIDPurgePending {
		store.state.SSIDConsent = false
		if store.state.PrivacyPurgeAction == "" {
			// Backward compatibility for state written before purge actions were
			// introduced: the old marker represented an SSID-only purge.
			store.state.PrivacyPurgeAction = PrivacyPurgeSSID
		}
	} else {
		store.state.PrivacyPurgeAction = ""
	}
	if store.state.ConfigVersion < 1 {
		store.state.ConfigVersion = 1
	}
	store.state.Config = protocol.NormalizeConfig(store.state.Config)
	if store.state.LastSpeedTests == nil {
		store.state.LastSpeedTests = map[string]time.Time{}
	}
	if store.state.HandledSpeedRequests == nil {
		store.state.HandledSpeedRequests = map[string]int64{}
	}
	if store.state.NetworkIdentityKey == "" {
		key, err := newRandomKey()
		if err != nil {
			return nil, err
		}
		store.state.NetworkIdentityKey = key
	}
	if !store.state.LastSpeedTest.IsZero() && store.state.LastSpeedTests["content"].IsZero() {
		store.state.LastSpeedTests["content"] = store.state.LastSpeedTest
	}
	if _, err := store.write(store.state); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *Store) Snapshot() State {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return cloneState(s.state)
}

func cloneState(value State) State {
	out := value
	out.Config.ProbeTargets = append([]string(nil), value.Config.ProbeTargets...)
	out.Config.DNSDomains = append([]string(nil), value.Config.DNSDomains...)
	out.Config.DNSResolvers = append([]string(nil), value.Config.DNSResolvers...)
	out.Config.HTTPTargets = append([]string(nil), value.Config.HTTPTargets...)
	out.Config.SpeedTestWindows = append([]string(nil), value.Config.SpeedTestWindows...)
	out.Config.SpeedProfiles = cloneSpeedProfiles(value.Config.SpeedProfiles)
	if value.Config.HouseholdMix != nil {
		mix := *value.Config.HouseholdMix
		out.Config.HouseholdMix = &mix
	}
	if value.HouseholdMix != nil {
		mix := *value.HouseholdMix
		out.HouseholdMix = &mix
	}
	out.LastSpeedTests = make(map[string]time.Time, len(value.LastSpeedTests))
	maps.Copy(out.LastSpeedTests, value.LastSpeedTests)
	out.HandledSpeedRequests = make(map[string]int64, len(value.HandledSpeedRequests))
	maps.Copy(out.HandledSpeedRequests, value.HandledSpeedRequests)
	return out
}

func (s *Store) commitLocked(change func(*State) error) error {
	next := cloneState(s.state)
	if err := change(&next); err != nil {
		return err
	}
	committed, err := s.write(next)
	if committed {
		s.state = next
	}
	if err != nil {
		return err
	}
	return nil
}

func (s *Store) SetEnrollment(controllerURL string, enrolled protocol.EnrollmentResponse) error {
	if err := protocol.ValidateIngestURL(controllerURL, enrolled.IngestURL); err != nil {
		return fmt.Errorf("validate enrollment ingest URL: %w", err)
	}
	identityKey, err := newRandomKey()
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitLocked(func(next *State) error {
		next.ControllerURL = controllerURL
		next.SensorID = enrolled.SensorID
		next.SensorSecret = enrolled.SensorSecret
		next.SiteID = enrolled.SiteID
		next.IngestURL = enrolled.IngestURL
		next.Mode = ModeConnected
		next.Config = protocol.NormalizeConfig(enrolled.Config)
		next.ConfigVersion = enrolled.ConfigVersion
		next.SSIDConsent = false
		next.PausedUntil = time.Time{}
		next.RevokedAt = time.Time{}
		next.HandledSpeedRequests = map[string]int64{}
		next.NetworkIdentityKey = identityKey
		return nil
	})
}

// SetMode switches between local-only and controller-connected operation.
// Controller credentials are retained while local-only so switching back does
// not require a new invite. The service and spool enforce that local-only
// records can never be uploaded after the switch.
func (s *Store) SetMode(mode string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if mode != ModeStandalone && mode != ModeConnected {
		return errors.New("mode must be standalone or connected")
	}
	if mode == ModeConnected && !s.state.Claimed() {
		return errors.New("connect to an Axon controller before selecting connected mode")
	}
	return s.commitLocked(func(next *State) error {
		next.Mode = mode
		next.PausedUntil = time.Time{}
		return nil
	})
}

// ValidUpdateChannel reports whether channel names a published release stream.
func ValidUpdateChannel(channel string) bool {
	switch channel {
	case UpdateChannelMain, UpdateChannelBeta, UpdateChannelAlpha:
		return true
	}
	return false
}

// SetUpdateChannel records the release stream to follow. An empty channel
// returns the installation to the default stream.
func (s *Store) SetUpdateChannel(channel string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if channel != "" && !ValidUpdateChannel(channel) {
		return errors.New("update channel must be main, beta, or alpha")
	}
	return s.commitLocked(func(next *State) error {
		next.UpdateChannel = channel
		return nil
	})
}

// SetHouseholdMix stores the local mix override; nil clears it so the
// controller configuration or the catalog default applies again.
func (s *Store) SetHouseholdMix(mix *quality.HouseholdMix) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitLocked(func(next *State) error {
		next.HouseholdMix = nil
		if mix != nil {
			normalized := quality.NormalizeHouseholdMix(*mix)
			next.HouseholdMix = &normalized
		}
		return nil
	})
}

func (s *Store) SetSSIDConsent(enabled bool) error {
	if !enabled {
		return s.BeginPrivacyPurge(PrivacyPurgeSSID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitLocked(func(next *State) error {
		if enabled && next.SSIDPurgePending {
			return errors.New("SSID cleanup is still pending")
		}
		next.SSIDConsent = true
		return nil
	})
}

// BeginSSIDPurge durably withdraws consent before any destructive cleanup. The
// pending bit keeps uploads fail-closed until CompleteSSIDPurge commits.
func (s *Store) BeginSSIDPurge() error {
	return s.BeginPrivacyPurge(PrivacyPurgeSSID)
}

// BeginPrivacyPurge durably withdraws consent and records the cleanup intent.
// Repeated calls may strengthen an existing action but never downgrade it.
func (s *Store) BeginPrivacyPurge(action PrivacyPurgeAction) error {
	if !validPrivacyPurgeAction(action) {
		return fmt.Errorf("invalid privacy purge action %q", action)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneState(s.state)
	beginPrivacyPurge(&next, action)
	sentinelErr := s.writePrivacyPurgeSentinel(next.PrivacyPurgeAction)
	committed, writeErr := s.write(next)
	// A durable sentinel or a committed state file backs this in-memory block.
	// Even if both stores fail, keeping the process fail-closed allows a later
	// retry to persist the action before any upload can resume.
	s.state = next
	if writeErr == nil {
		return nil
	}
	if sentinelErr == nil {
		return writeErr
	}
	if committed {
		return writeErr
	}
	return errors.Join(writeErr, sentinelErr)
}

func (s *Store) CompleteSSIDPurge() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := cloneState(s.state)
	if !current.SSIDPurgePending {
		return nil
	}
	if err := s.writePrivacyPurgeSentinel(current.PrivacyPurgeAction); err != nil {
		return fmt.Errorf("refresh privacy purge sentinel: %w", err)
	}
	next := cloneState(current)
	next.SSIDPurgePending = false
	next.PrivacyPurgeAction = ""
	committed, err := s.write(next)
	if err != nil {
		s.state = current
		return err
	}
	if !committed {
		s.state = current
		return errors.New("complete privacy purge did not commit state")
	}
	s.state = next
	removed, err := s.removePrivacyPurgeSentinel()
	if err != nil {
		s.state = current
		return err
	}
	if removed {
		if err := s.files.syncDirectory(filepath.Dir(s.path)); err != nil {
			s.state = current
			return fmt.Errorf("sync privacy purge sentinel removal: %w", err)
		}
	}
	return nil
}

func beginPrivacyPurge(next *State, action PrivacyPurgeAction) {
	next.SSIDConsent = false
	next.SSIDPurgePending = true
	if privacyPurgePriority(action) > privacyPurgePriority(next.PrivacyPurgeAction) {
		next.PrivacyPurgeAction = action
	}
}

func validPrivacyPurgeAction(action PrivacyPurgeAction) bool {
	return action == PrivacyPurgeSSID || action == PrivacyPurgeDisconnect || action == PrivacyPurgeRevocation
}

func privacyPurgePriority(action PrivacyPurgeAction) int {
	switch action {
	case PrivacyPurgeSSID:
		return 1
	case PrivacyPurgeDisconnect:
		return 2
	case PrivacyPurgeRevocation:
		return 3
	default:
		return 0
	}
}

func (s *Store) SetConfig(config protocol.SensorConfig, version int) error {
	if version < 1 {
		return errors.New("config version must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if version < s.state.ConfigVersion {
		return nil
	}
	return s.commitLocked(func(next *State) error {
		next.Config = protocol.NormalizeConfig(config)
		next.ConfigVersion = version
		return nil
	})
}

func (s *Store) SetPausedUntil(until time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitLocked(func(next *State) error {
		next.PausedUntil = until.UTC()
		return nil
	})
}

// AcceptSpeedTestRequest durably consumes an authenticated controller nonce.
// Retaining only unexpired entries bounds state while preventing replay after
// a service restart.
func (s *Store) AcceptSpeedTestRequest(nonce string, expiresAt, now time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := cloneState(s.state)
	if next.HandledSpeedRequests == nil {
		next.HandledSpeedRequests = map[string]int64{}
	}
	for handledNonce, expiry := range next.HandledSpeedRequests {
		if expiry < now.Unix() {
			delete(next.HandledSpeedRequests, handledNonce)
		}
	}
	if _, exists := next.HandledSpeedRequests[nonce]; exists {
		return false, nil
	}
	if len(next.HandledSpeedRequests) >= 32 {
		var oldest string
		var oldestExpiry int64
		for handledNonce, expiry := range next.HandledSpeedRequests {
			if oldest == "" || expiry < oldestExpiry {
				oldest, oldestExpiry = handledNonce, expiry
			}
		}
		delete(next.HandledSpeedRequests, oldest)
	}
	next.HandledSpeedRequests[nonce] = expiresAt.Unix()
	committed, err := s.write(next)
	if committed {
		s.state = next
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) RemainingDataBudget(now time.Time) uint64 {
	s.mu.RLock()
	defer s.mu.RUnlock()
	dailyRemaining, monthlyRemaining := s.remainingDataBudgetsLocked(now, s.state.Config.DailyDataBudgetMB, s.state.Config.MonthlyDataBudgetMB)
	return min(dailyRemaining, monthlyRemaining)
}

func (s *Store) RemainingDataBudgets(now time.Time) (uint64, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.remainingDataBudgetsLocked(now, s.state.Config.DailyDataBudgetMB, s.state.Config.MonthlyDataBudgetMB)
}

func (s *Store) RemainingDataBudgetsFor(now time.Time, dailyLimitMB, monthlyLimitMB int) (uint64, uint64) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.remainingDataBudgetsLocked(now, dailyLimitMB, monthlyLimitMB)
}

func (s *Store) remainingDataBudgetsLocked(now time.Time, dailyLimitMB, monthlyLimitMB int) (uint64, uint64) {
	dailyMaximum := uint64(max(dailyLimitMB, 0)) << 20
	dailyRemaining := dailyMaximum
	if s.state.BudgetDay == now.Format("2006-01-02") {
		if s.state.BudgetBytes >= dailyMaximum {
			dailyRemaining = 0
		} else {
			dailyRemaining = dailyMaximum - s.state.BudgetBytes
		}
	}
	monthlyMaximum := uint64(max(monthlyLimitMB, 0)) << 20
	monthlyRemaining := monthlyMaximum
	if s.state.BudgetMonth == now.Format("2006-01") {
		if s.state.MonthlyBudgetBytes >= monthlyMaximum {
			monthlyRemaining = 0
		} else {
			monthlyRemaining = monthlyMaximum - s.state.MonthlyBudgetBytes
		}
	}
	return dailyRemaining, monthlyRemaining
}

// ReserveSpeedTestProfile charges the complete effective test envelope before
// any network traffic starts. If the process is killed mid-test the durable
// ledger therefore remains conservative instead of silently forgetting bytes.
func (s *Store) ReserveSpeedTestProfile(at time.Time, bytes uint64) error {
	config := s.Snapshot().Config
	return s.ReserveSpeedTestProfileFor(at, bytes, config.DailyDataBudgetMB, config.MonthlyDataBudgetMB)
}

func (s *Store) ReserveSpeedTestProfileFor(at time.Time, bytes uint64, dailyLimitMB, monthlyLimitMB int) error {
	return s.reserveSpeedTestBytes(at, bytes, dailyLimitMB, monthlyLimitMB, false)
}

// ReserveSpeedTestOverdraw charges a reservation even when it exceeds the
// remaining ledgers. It exists for user-initiated tests, which may knowingly
// spend past the budget; the overdraw still lands in the durable ledger, so
// scheduled tests stay deferred until the day or month rolls over.
func (s *Store) ReserveSpeedTestOverdraw(at time.Time, bytes uint64, dailyLimitMB, monthlyLimitMB int) error {
	return s.reserveSpeedTestBytes(at, bytes, dailyLimitMB, monthlyLimitMB, true)
}

func (s *Store) reserveSpeedTestBytes(at time.Time, bytes uint64, dailyLimitMB, monthlyLimitMB int, allowOverdraw bool) error {
	if bytes == 0 {
		return errors.New("speed-test reservation must be positive")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	dailyRemaining, monthlyRemaining := s.remainingDataBudgetsLocked(at, dailyLimitMB, monthlyLimitMB)
	if !allowOverdraw && bytes > min(dailyRemaining, monthlyRemaining) {
		return errors.New("speed-test reservation exceeds remaining data budget")
	}
	return s.commitLocked(func(next *State) error {
		rollBudgets(next, at)
		next.BudgetBytes += bytes
		next.MonthlyBudgetBytes += bytes
		return nil
	})
}

// ReconcileSpeedTestProfile refunds unused reserved bytes after a clean return
// and records spacing only for a completed result. A crash before this method
// deliberately retains the full reservation.
func (s *Store) ReconcileSpeedTestProfile(at time.Time, profile string, reserved, used uint64, completed bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitLocked(func(next *State) error {
		used = min(used, reserved)
		refund := reserved - used
		if next.BudgetDay == at.Format("2006-01-02") {
			next.BudgetBytes -= min(refund, next.BudgetBytes)
		}
		if next.BudgetMonth == at.Format("2006-01") {
			next.MonthlyBudgetBytes -= min(refund, next.MonthlyBudgetBytes)
		}
		if completed {
			next.LastSpeedTest = at.UTC()
			if next.LastSpeedTests == nil {
				next.LastSpeedTests = map[string]time.Time{}
			}
			next.LastSpeedTests[profile] = at.UTC()
		}
		return nil
	})
}

func (s *Store) RecordSpeedTest(at time.Time, bytesUsed uint64) error {
	return s.RecordSpeedTestProfile(at, "content", bytesUsed)
}

func (s *Store) RecordSpeedTestProfile(at time.Time, profile string, bytesUsed uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitLocked(func(next *State) error {
		rollBudgets(next, at)
		next.BudgetBytes += bytesUsed
		next.MonthlyBudgetBytes += bytesUsed
		next.LastSpeedTest = at.UTC()
		if next.LastSpeedTests == nil {
			next.LastSpeedTests = map[string]time.Time{}
		}
		next.LastSpeedTests[profile] = at.UTC()
		return nil
	})
}

func rollBudgets(value *State, at time.Time) {
	day := at.Format("2006-01-02")
	if value.BudgetDay != day {
		value.BudgetDay, value.BudgetBytes = day, 0
	}
	month := at.Format("2006-01")
	if value.BudgetMonth != month {
		value.BudgetMonth, value.MonthlyBudgetBytes = month, 0
	}
}

// WipeCredentials removes all controller-issued identity material. The random
// installation UID is retained so a retry of an enrollment request is stable.
func (s *Store) WipeCredentials(revoked bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitLocked(func(next *State) error {
		next.ControllerURL = ""
		next.Mode = ModeStandalone
		next.SensorID = ""
		next.SensorSecret = ""
		next.SiteID = ""
		next.IngestURL = ""
		next.Config = protocol.DefaultConfig()
		next.ConfigVersion = 1
		next.SSIDConsent = false
		next.PausedUntil = time.Time{}
		next.LastSpeedTest = time.Time{}
		next.LastSpeedTests = map[string]time.Time{}
		next.BudgetDay = ""
		next.BudgetBytes = 0
		next.BudgetMonth = ""
		next.MonthlyBudgetBytes = 0
		next.HandledSpeedRequests = map[string]int64{}
		next.NetworkIdentityKey = ""
		if revoked {
			next.RevokedAt = time.Now().UTC()
		} else {
			next.RevokedAt = time.Time{}
		}
		return nil
	})
}

func newRandomKey() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", fmt.Errorf("generate local identity key: %w", err)
	}
	return hex.EncodeToString(value), nil
}

func cloneSpeedProfiles(values []protocol.SpeedProfileConfig) []protocol.SpeedProfileConfig {
	out := append([]protocol.SpeedProfileConfig(nil), values...)
	for index := range out {
		out[index].Windows = append([]string(nil), values[index].Windows...)
	}
	return out
}

func (s *Store) readPrivacyPurgeSentinel() (PrivacyPurgeAction, bool, error) {
	data, err := os.ReadFile(filepath.Join(filepath.Dir(s.path), PrivacyPurgeFilename))
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read privacy purge sentinel: %w", err)
	}
	var value struct {
		Action PrivacyPurgeAction `json:"action"`
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return "", false, fmt.Errorf("decode privacy purge sentinel: %w", err)
	}
	if !validPrivacyPurgeAction(value.Action) {
		return "", false, fmt.Errorf("decode privacy purge sentinel: invalid action %q", value.Action)
	}
	return value.Action, true, nil
}

func (s *Store) writePrivacyPurgeSentinel(action PrivacyPurgeAction) error {
	if !validPrivacyPurgeAction(action) {
		return fmt.Errorf("invalid privacy purge sentinel action %q", action)
	}
	data, err := json.Marshal(struct {
		Action PrivacyPurgeAction `json:"action"`
	}{Action: action})
	if err != nil {
		return fmt.Errorf("encode privacy purge sentinel: %w", err)
	}
	directory := filepath.Dir(s.path)
	file, err := s.files.createTemp(directory, ".privacy-purge-*.tmp")
	if err != nil {
		return fmt.Errorf("create privacy purge sentinel: %w", err)
	}
	temporary := file.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = s.files.remove(temporary)
		}
	}()
	if err := s.files.chmod(temporary, 0o600); err != nil {
		return fmt.Errorf("secure privacy purge sentinel: %w", errors.Join(err, file.Close()))
	}
	payload := make([]byte, len(data)+1)
	copy(payload, data)
	payload[len(data)] = '\n'
	written, writeErr := file.Write(payload)
	if writeErr == nil && written != len(payload) {
		writeErr = io.ErrShortWrite
	}
	syncErr, closeErr := file.Sync(), file.Close()
	if err := errors.Join(writeErr, syncErr, closeErr); err != nil {
		return fmt.Errorf("write privacy purge sentinel: %w", err)
	}
	target := filepath.Join(directory, PrivacyPurgeFilename)
	if err := s.files.replace(temporary, target); err != nil {
		return fmt.Errorf("replace privacy purge sentinel: %w", err)
	}
	cleanup = false
	if err := s.files.syncDirectory(directory); err != nil {
		return fmt.Errorf("sync privacy purge sentinel: %w", err)
	}
	return nil
}

func (s *Store) removePrivacyPurgeSentinel() (bool, error) {
	err := s.files.remove(filepath.Join(filepath.Dir(s.path), PrivacyPurgeFilename))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("remove privacy purge sentinel: %w", err)
	}
	return true, nil
}

func (s *Store) writeState(value State) (bool, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encode state: %w", err)
	}
	directory := filepath.Dir(s.path)
	file, err := s.files.createTemp(directory, ".state-*.tmp")
	if err != nil {
		return false, fmt.Errorf("create state: %w", err)
	}
	temporary := file.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = s.files.remove(temporary)
		}
	}()
	if err := s.files.chmod(temporary, 0o600); err != nil {
		return false, fmt.Errorf("secure state: %w", errors.Join(err, file.Close()))
	}
	payload := make([]byte, len(data)+1)
	copy(payload, data)
	payload[len(data)] = '\n'
	written, err := file.Write(payload)
	if written != len(payload) {
		err = errors.Join(err, io.ErrShortWrite)
	}
	if err != nil {
		return false, fmt.Errorf("write state: %w", errors.Join(err, file.Close()))
	}
	if err := file.Sync(); err != nil {
		return false, fmt.Errorf("sync state: %w", errors.Join(err, file.Close()))
	}
	if err := file.Close(); err != nil {
		return false, fmt.Errorf("close state: %w", err)
	}
	if err := s.files.replace(temporary, s.path); err != nil {
		return false, fmt.Errorf("replace state: %w", err)
	}
	cleanup = false
	if err := s.files.syncDirectory(directory); err != nil {
		return true, fmt.Errorf("sync state directory: %w", err)
	}
	return true, nil
}

func DefaultDir() (string, error) {
	if configured := os.Getenv("AXON_PULSE_STATE_DIR"); configured != "" {
		return filepath.Clean(configured), nil
	}
	if runtime.GOOS == "linux" && isPrivileged() {
		return "/var/lib/axon-pulse", nil
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find user config directory: %w", err)
	}
	return filepath.Join(configDir, "axon-pulse"), nil
}

func SocketPath(dir string) string {
	if configured := os.Getenv("AXON_PULSE_SOCKET"); configured != "" {
		return configured
	}
	return defaultSocketPath(dir)
}

func newUUIDv4() (string, error) {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		return "", fmt.Errorf("generate sensor UID: %w", err)
	}
	value[6] = (value[6] & 0x0f) | 0x40
	value[8] = (value[8] & 0x3f) | 0x80
	encoded := make([]byte, 36)
	hex.Encode(encoded[0:8], value[0:4])
	encoded[8] = '-'
	hex.Encode(encoded[9:13], value[4:6])
	encoded[13] = '-'
	hex.Encode(encoded[14:18], value[6:8])
	encoded[18] = '-'
	hex.Encode(encoded[19:23], value[8:10])
	encoded[23] = '-'
	hex.Encode(encoded[24:36], value[10:16])
	return string(encoded), nil
}
