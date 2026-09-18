package state

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Taurine-Technology/axon-pulse/internal/protocol"
)

type (
	failingStateFile struct {
		stateFile

		writeErr error
		syncErr  error
		closeErr error
	}
)

func (f *failingStateFile) Write(data []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return f.stateFile.Write(data)
}

func (f *failingStateFile) Sync() error {
	if f.syncErr != nil {
		return f.syncErr
	}
	return f.stateFile.Sync()
}

func (f *failingStateFile) Close() error {
	return errors.Join(f.stateFile.Close(), f.closeErr)
}

func TestStorePersistsStableUIDAndCredentialsSecurely(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	uid := store.Snapshot().SensorUID
	if uid == "" {
		t.Fatal("sensor UID was not generated")
	}
	err = store.SetEnrollment("https://controller.example", protocol.EnrollmentResponse{
		SensorID: "sensor", SensorSecret: "secret", SiteID: "site",
		IngestURL: "https://controller.example" + protocol.IngestPath,
		Config:    protocol.DefaultConfig(), ConfigVersion: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Snapshot().SensorUID != uid || !reopened.Snapshot().Claimed() {
		t.Fatalf("state did not survive reopen: %+v", reopened.Snapshot())
	}
	info, err := os.Stat(filepath.Join(dir, StateFilename))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" {
		// Windows has no unix permission bits; the state file is instead
		// protected by the per-user profile directory ACLs.
		if got := info.Mode().Perm(); got != 0o600 {
			t.Fatalf("state mode = %o, want 600", got)
		}
	}
}

func TestWipeCredentialsPreservesUID(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	uid := store.Snapshot().SensorUID
	initialIdentityKey := store.Snapshot().NetworkIdentityKey
	if initialIdentityKey == "" {
		t.Fatal("device-local identity key was not generated")
	}
	if err := store.SetSSIDConsent(true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPausedUntil(time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := store.WipeCredentials(true); err != nil {
		t.Fatal(err)
	}
	got := store.Snapshot()
	if got.SensorUID != uid || got.Claimed() || got.RevokedAt.IsZero() || got.Paused(time.Now()) || got.SSIDConsent || got.NetworkIdentityKey != "" {
		t.Fatalf("unexpected wiped state: %+v", got)
	}
	if err := store.SetEnrollment("https://controller.example", protocol.EnrollmentResponse{SensorID: "new-sensor", SensorSecret: "new-secret", SiteID: "site", IngestURL: "https://controller.example/ingest", Config: protocol.DefaultConfig(), ConfigVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if rotated := store.Snapshot().NetworkIdentityKey; rotated == "" || rotated == initialIdentityKey {
		t.Fatalf("identity key was not rotated on re-enrollment: %q", rotated)
	}
}

func TestSSIDConsentIsLocalAndDefaultsOffForEnrollment(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSSIDConsent(true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetEnrollment("https://controller.example", protocol.EnrollmentResponse{
		SensorID: "sensor", SensorSecret: "secret", SiteID: "site", IngestURL: "https://controller.example/ingest",
		Config: protocol.SensorConfig{SendSSID: true}, ConfigVersion: 2,
	}); err != nil {
		t.Fatal(err)
	}
	if store.Snapshot().SSIDConsent {
		t.Fatal("new enrollment inherited SSID consent")
	}
	if err := store.SetSSIDConsent(true); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reopened.Snapshot().SSIDConsent {
		t.Fatal("SSID consent did not persist")
	}
}

func TestOperatingModeSwitchRetainsClaim(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if store.Snapshot().Mode != ModeSetup {
		t.Fatalf("initial mode = %q", store.Snapshot().Mode)
	}
	if err := store.SetMode(ModeStandalone); err != nil {
		t.Fatal(err)
	}
	if !store.Snapshot().Operational() || store.Snapshot().Claimed() {
		t.Fatalf("standalone state = %+v", store.Snapshot())
	}
	if err := store.SetMode(ModeConnected); err == nil {
		t.Fatal("unclaimed store entered connected mode")
	}
	if err := store.SetEnrollment("https://controller.example", protocol.EnrollmentResponse{SensorID: "sensor", SensorSecret: "secret", SiteID: "site", IngestURL: "https://controller.example/ingest", Config: protocol.DefaultConfig(), ConfigVersion: 1}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetMode(ModeStandalone); err != nil {
		t.Fatal(err)
	}
	if !store.Snapshot().Claimed() || store.Snapshot().Mode != ModeStandalone {
		t.Fatalf("local switch destroyed claim: %+v", store.Snapshot())
	}
	if err := store.SetMode(ModeConnected); err != nil || store.Snapshot().Mode != ModeConnected {
		t.Fatalf("connected switch = %+v, %v", store.Snapshot(), err)
	}
}

func TestSpeedTestReservationSurvivesRestartAndReconcilesUnusedBytes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	before := store.RemainingDataBudget(now)
	const reserved = uint64(10 << 20)
	if err := store.ReserveSpeedTestProfile(now, reserved); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.RemainingDataBudget(now); got != before-reserved {
		t.Fatalf("restart forgot reservation: remaining=%d want=%d", got, before-reserved)
	}
	const used = uint64(3 << 20)
	if err := reopened.ReconcileSpeedTestProfile(now, "content", reserved, used, false); err != nil {
		t.Fatal(err)
	}
	if got := reopened.RemainingDataBudget(now); got != before-used {
		t.Fatalf("reconciled remaining=%d want=%d", got, before-used)
	}
	if !reopened.Snapshot().LastSpeedTests["content"].IsZero() {
		t.Fatal("aborted test incorrectly satisfied the scheduling window")
	}
}

func TestRemainingDataBudgetsExposeDailyAndMonthlyLimitsSeparately(t *testing.T) {
	t.Parallel()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	if err := store.RecordSpeedTestProfile(now, "content", 10<<20); err != nil {
		t.Fatal(err)
	}
	defaults := protocol.DefaultConfig()
	daily, monthly := store.RemainingDataBudgets(now)
	if daily != uint64(defaults.DailyDataBudgetMB-10)<<20 || monthly != uint64(defaults.MonthlyDataBudgetMB-10)<<20 {
		t.Fatalf("remaining daily=%d monthly=%d", daily, monthly)
	}
}

func TestControllerSpeedRequestNoncePersistsAcrossRestart(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 18, 12, 0, 0, 0, time.UTC)
	expires := now.Add(5 * time.Minute)
	if accepted, err := store.AcceptSpeedTestRequest("nonce_1234567890", expires, now); err != nil || !accepted {
		t.Fatalf("first acceptance = %v, %v", accepted, err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if accepted, err := reopened.AcceptSpeedTestRequest("nonce_1234567890", expires, now); err != nil || accepted {
		t.Fatalf("replayed acceptance = %v, %v", accepted, err)
	}
}

func TestStateMutationsDoNotLeakWhenPersistenceFails(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	failure := errors.New("injected persistence failure")
	tests := []struct {
		name   string
		setup  func(*testing.T, *Store)
		mutate func(*Store) error
	}{
		{
			name: "scalar mode",
			mutate: func(store *Store) error {
				return store.SetMode(ModeStandalone)
			},
		},
		{
			name: "nested config slices",
			mutate: func(store *Store) error {
				config := protocol.DefaultConfig()
				config.ProbeTargets = []string{"anchor:9.9.9.9"}
				config.SpeedProfiles[0].Windows = []string{"01:00-02:00"}
				return store.SetConfig(config, 2)
			},
		},
		{
			name: "nonce map",
			mutate: func(store *Store) error {
				_, err := store.AcceptSpeedTestRequest("nonce_1234567890", now.Add(time.Minute), now)
				return err
			},
		},
		{
			name: "budget rollover",
			mutate: func(store *Store) error {
				return store.ReserveSpeedTestProfile(now, 1<<20)
			},
		},
		{
			name: "budget reconciliation",
			setup: func(t *testing.T, store *Store) {
				t.Helper()
				if err := store.ReserveSpeedTestProfile(now, 2<<20); err != nil {
					t.Fatal(err)
				}
			},
			mutate: func(store *Store) error {
				return store.ReconcileSpeedTestProfile(now, "content", 2<<20, 1<<20, true)
			},
		},
		{
			name: "credential wipe",
			setup: func(t *testing.T, store *Store) {
				t.Helper()
				err := store.SetEnrollment("https://controller.example", protocol.EnrollmentResponse{
					SensorID: "sensor", SensorSecret: "secret", SiteID: "site",
					IngestURL: "https://controller.example/ingest", Config: protocol.DefaultConfig(), ConfigVersion: 1,
				})
				if err != nil {
					t.Fatal(err)
				}
			},
			mutate: func(store *Store) error {
				return store.WipeCredentials(false)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			store, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if test.setup != nil {
				test.setup(t, store)
			}
			before := store.Snapshot()
			beforeFile, err := os.ReadFile(filepath.Join(dir, StateFilename))
			if err != nil {
				t.Fatal(err)
			}
			store.write = func(State) (bool, error) { return false, failure }
			if err := test.mutate(store); !errors.Is(err, failure) {
				t.Fatalf("mutation error = %v", err)
			}
			if got := store.Snapshot(); !reflect.DeepEqual(got, before) {
				t.Fatalf("failed mutation changed memory:\n got: %+v\nwant: %+v", got, before)
			}
			afterFile, err := os.ReadFile(filepath.Join(dir, StateFilename))
			if err != nil {
				t.Fatal(err)
			}
			if string(afterFile) != string(beforeFile) {
				t.Fatal("failed mutation changed state.json")
			}

			store.write = store.writeState
			checkpoint := now.Add(time.Hour)
			if err := store.SetPausedUntil(checkpoint); err != nil {
				t.Fatal(err)
			}
			expected := before
			expected.PausedUntil = checkpoint
			reopened, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			if got := reopened.Snapshot(); !reflect.DeepEqual(got, expected) {
				t.Fatalf("failed data piggybacked on a later commit:\n got: %+v\nwant: %+v", got, expected)
			}
		})
	}
}

func TestPostRenameFailureKeepsMemoryAlignedWithDisk(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	postCommitFailure := errors.New("directory sync failed after rename")
	store.files.syncDirectory = func(string) error { return postCommitFailure }
	if err := store.SetMode(ModeStandalone); !errors.Is(err, postCommitFailure) {
		t.Fatalf("SetMode error = %v", err)
	}
	if store.Snapshot().Mode != ModeStandalone {
		t.Fatal("memory remained older than the renamed state file")
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Snapshot().Mode != ModeStandalone {
		t.Fatal("renamed state file did not contain the committed candidate")
	}
}

func TestDurableWriterFailuresLeaveMemoryAndStateFileUnchanged(t *testing.T) {
	t.Parallel()
	failure := errors.New("injected state writer failure")
	tests := []struct {
		name   string
		inject func(*Store)
	}{
		{
			name: "write",
			inject: func(store *Store) {
				createTemp := store.files.createTemp
				store.files.createTemp = func(dir, pattern string) (stateFile, error) {
					file, err := createTemp(dir, pattern)
					return &failingStateFile{stateFile: file, writeErr: failure}, err
				}
			},
		},
		{
			name: "sync",
			inject: func(store *Store) {
				createTemp := store.files.createTemp
				store.files.createTemp = func(dir, pattern string) (stateFile, error) {
					file, err := createTemp(dir, pattern)
					return &failingStateFile{stateFile: file, syncErr: failure}, err
				}
			},
		},
		{
			name: "close",
			inject: func(store *Store) {
				createTemp := store.files.createTemp
				store.files.createTemp = func(dir, pattern string) (stateFile, error) {
					file, err := createTemp(dir, pattern)
					return &failingStateFile{stateFile: file, closeErr: failure}, err
				}
			},
		},
		{
			name: "replace",
			inject: func(store *Store) {
				store.files.replace = func(string, string) error { return failure }
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			store, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			before := store.Snapshot()
			beforeFile, err := os.ReadFile(filepath.Join(dir, StateFilename))
			if err != nil {
				t.Fatal(err)
			}
			test.inject(store)
			if err := store.SetMode(ModeStandalone); !errors.Is(err, failure) {
				t.Fatalf("SetMode error = %v", err)
			}
			if got := store.Snapshot(); !reflect.DeepEqual(got, before) {
				t.Fatalf("failed durable write changed memory:\n got: %+v\nwant: %+v", got, before)
			}
			afterFile, err := os.ReadFile(filepath.Join(dir, StateFilename))
			if err != nil {
				t.Fatal(err)
			}
			if string(afterFile) != string(beforeFile) {
				t.Fatal("failed durable write changed state.json")
			}
			matches, err := filepath.Glob(filepath.Join(dir, ".state-*.tmp"))
			if err != nil {
				t.Fatal(err)
			}
			if len(matches) != 0 {
				t.Fatalf("failed durable write left temporary files: %v", matches)
			}
		})
	}
}

func TestSSIDPurgeMarkerIsDurableAndOnlyCompletionClearsIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetSSIDConsent(true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSSIDConsent(false); err != nil {
		t.Fatal(err)
	}
	if err := store.SetSSIDConsent(true); err == nil {
		t.Fatal("consent was re-enabled while privacy cleanup was pending")
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Snapshot(); got.SSIDConsent || !got.SSIDPurgePending {
		t.Fatalf("reopened purge state = %+v", got)
	} else if got.PrivacyPurgeAction != PrivacyPurgeSSID {
		t.Fatalf("reopened purge action = %q", got.PrivacyPurgeAction)
	}
	if err := reopened.CompleteSSIDPurge(); err != nil {
		t.Fatal(err)
	}
	if reopened.Snapshot().SSIDPurgePending {
		t.Fatal("completed privacy cleanup left the fail-closed marker set")
	}
}

func TestPrivacyPurgeActionIsDurableAndCannotBeDowngraded(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginPrivacyPurge(PrivacyPurgeDisconnect); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginPrivacyPurge(PrivacyPurgeSSID); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginPrivacyPurge(PrivacyPurgeRevocation); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginPrivacyPurge(PrivacyPurgeDisconnect); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.Snapshot()
	if !got.SSIDPurgePending || got.PrivacyPurgeAction != PrivacyPurgeRevocation {
		t.Fatalf("reopened purge state = %+v", got)
	}
	if err := reopened.BeginPrivacyPurge("unknown"); err == nil {
		t.Fatal("invalid privacy action was accepted")
	}
}

func TestPrivacyPurgeSentinelRecoversFailedStateCommit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	originalWrite := store.write
	injected := errors.New("injected state commit failure")
	store.write = func(State) (bool, error) { return false, injected }
	if err := store.BeginPrivacyPurge(PrivacyPurgeRevocation); !errors.Is(err, injected) {
		t.Fatalf("BeginPrivacyPurge error = %v", err)
	}
	store.write = originalWrite
	if got := store.Snapshot(); !got.SSIDPurgePending || got.PrivacyPurgeAction != PrivacyPurgeRevocation {
		t.Fatalf("in-memory fail-closed state = %+v", got)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	got := reopened.Snapshot()
	if !got.SSIDPurgePending || got.PrivacyPurgeAction != PrivacyPurgeRevocation {
		t.Fatalf("sentinel-recovered state = %+v", got)
	}
	if err := reopened.CompleteSSIDPurge(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, PrivacyPurgeFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed purge sentinel still exists: %v", err)
	}
}

func TestOpenRejectsUnknownPrivacyPurgeAction(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	contents := []byte(`{"sensor_uid":"sensor","mode":"setup","ssid_purge_pending":true,"privacy_purge_action":"future-action"}`)
	if err := os.WriteFile(filepath.Join(dir, StateFilename), contents, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir); err == nil || !strings.Contains(err.Error(), "invalid privacy purge action") {
		t.Fatalf("Open error = %v", err)
	}
}

func TestUpdateChannelPersistsAndValidates(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Snapshot().UpdateChannel; got != "" {
		t.Fatalf("fresh store follows %q, want the default stream", got)
	}
	if err := store.SetUpdateChannel("nightly"); err == nil {
		t.Fatal("unknown stream was accepted")
	}
	if err := store.SetUpdateChannel(UpdateChannelBeta); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := reopened.Snapshot().UpdateChannel; got != UpdateChannelBeta {
		t.Fatalf("reopened stream = %q, want beta", got)
	}
	if err := reopened.SetUpdateChannel(""); err != nil || reopened.Snapshot().UpdateChannel != "" {
		t.Fatalf("clearing the stream = %v, state %+v", err, reopened.Snapshot())
	}
}
