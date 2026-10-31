package main

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const rollbackJournalDir = ".rollback"

// rollbackJournal records the paths a coordinated code + data rollback moved, so a crash mid-swap can be undone on startup.
type rollbackJournal struct {
	Name       string `json:"name"`
	Snapshot   string `json:"snapshot"`
	AppDir     string `json:"appDir"`
	AppBackup  string `json:"appBackup"`
	DataDir    string `json:"dataDir,omitempty"`
	DataBackup string `json:"dataBackup,omitempty"`
	DB         string `json:"db,omitempty"`
	DBBackup   string `json:"dbBackup,omitempty"`
	StagedApp  string `json:"stagedApp"`
	StagedData string `json:"stagedData,omitempty"`
	StagedDB   string `json:"stagedDB,omitempty"`
	Committed  bool   `json:"committed"`
}

func writeRollbackJournal(stateDir string, journal rollbackJournal) (string, error) {
	dir := filepath.Join(stateDir, rollbackJournalDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := filepath.Join(dir, fmt.Sprintf("%s.%d.json", journal.Name, time.Now().UnixNano()))
	data, err := json.Marshal(journal)
	if err != nil {
		return "", err
	}
	if err := writeFileAtomic(path, append(data, '\n')); err != nil {
		return "", err
	}
	return path, nil
}

func saveRollbackJournal(path string, journal rollbackJournal) error {
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(data, '\n'))
}

// moveAside renames a live path to its backup and reports whether it existed.
func moveAside(live, backup string) (bool, error) {
	if _, err := os.Stat(live); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	if err := os.Rename(live, backup); err != nil {
		return false, err
	}
	return true, nil
}

// restoreAside puts a backup back over the live path, removing whatever is there now.
func restoreAside(live, backup string, moved bool) error {
	if !moved {
		return nil
	}
	if err := os.RemoveAll(live); err != nil {
		return err
	}
	return os.Rename(backup, live)
}

func removeSQLiteSidecars(path string) {
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}

// restoreDB puts a database backup back over the live file and drops stale WAL sidecars.
func restoreDB(live, backup string) error {
	if _, err := os.Stat(backup); err != nil {
		return nil
	}
	removeSQLiteSidecars(live)
	if err := os.RemoveAll(live); err != nil {
		return err
	}
	return os.Rename(backup, live)
}

func removeIfExists(path string) {
	if path == "" {
		return
	}
	_ = os.RemoveAll(path)
}

func existsPath(path string) bool {
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// pendingRollback reports an unfinished journal, so a compensation failure is resolved by a restart before another attempt.
func pendingRollback(stateDir, name string) bool {
	entries, err := os.ReadDir(filepath.Join(stateDir, rollbackJournalDir))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), name+".") && filepath.Ext(entry.Name()) == ".json" {
			return true
		}
	}
	return false
}

// recoverRollbacks undoes uncommitted swaps and cleans committed ones before apps load.
func recoverRollbacks(stateDir string, backups *backupManager) error {
	dir := filepath.Join(stateDir, rollbackJournalDir)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var journal rollbackJournal
		if err := json.Unmarshal(data, &journal); err != nil {
			log.Printf("rollback recovery %s: %v", entry.Name(), err)
			continue
		}

		if journal.Committed {
			if backups != nil && journal.Snapshot != "" {
				if err := backups.markCurrent(journal.Name, journal.Snapshot); err != nil {
					log.Printf("rollback recovery %s: mark current: %v", journal.Name, err)
				}
			}
		} else {
			// The app was stopped in the crashed process, so put the old halves back before loadApps starts anything.
			if err := restoreDB(journal.DB, journal.DBBackup); err != nil {
				return fmt.Errorf("rollback recovery %s: restore database: %w", journal.Name, err)
			}
			if err := restoreAside(journal.DataDir, journal.DataBackup, existsPath(journal.DataBackup)); err != nil {
				return fmt.Errorf("rollback recovery %s: restore data: %w", journal.Name, err)
			}
			if err := restoreAside(journal.AppDir, journal.AppBackup, existsPath(journal.AppBackup)); err != nil {
				return fmt.Errorf("rollback recovery %s: restore app: %w", journal.Name, err)
			}
			log.Printf("rollback %s: recovered the previous code and data", journal.Name)
		}

		removeIfExists(journal.StagedApp)
		removeIfExists(journal.StagedData)
		removeIfExists(journal.StagedDB)
		removeIfExists(journal.AppBackup)
		removeIfExists(journal.DataBackup)
		removeIfExists(journal.DBBackup)
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

// failRollback restores the previous halves and process, then reports the original error.
func (s *supervisor) failRollback(w http.ResponseWriter, journalPath string, journal rollbackJournal, old *app, running bool, cause error) {
	if s.storage != nil {
		s.storage.opMu.Lock()
		s.storage.drop(journal.Name)
		removeSQLiteSidecars(journal.DB)
	}
	restoreErr := restoreDB(journal.DB, journal.DBBackup)
	if s.storage != nil {
		s.storage.opMu.Unlock()
	}
	if restoreErr == nil {
		restoreErr = restoreAside(journal.DataDir, journal.DataBackup, existsPath(journal.DataBackup))
	}
	if restoreErr == nil {
		restoreErr = restoreAside(journal.AppDir, journal.AppBackup, existsPath(journal.AppBackup))
	}
	if restoreErr != nil {
		// Keep the journal so startup recovery can retry before apps load.
		log.Printf("rollback %s: compensation failed: %v", journal.Name, restoreErr)
		writeError(w, http.StatusInternalServerError, "rollback failed and compensation is incomplete: "+restoreErr.Error())
		return
	}
	if err := os.Remove(journalPath); err != nil && !os.IsNotExist(err) {
		log.Printf("rollback %s: remove journal: %v", journal.Name, err)
	}
	if running {
		if err := s.startStopped(old); err != nil {
			log.Printf("rollback %s: previous version did not restart: %v", journal.Name, err)
			writeError(w, http.StatusBadGateway, "rollback failed, previous code restored but did not restart: "+err.Error())
			return
		}
	}
	writeError(w, http.StatusBadGateway, cause.Error())
}

// rollbackPlan carries the staged paths and journal through the code, data and database swaps.
type rollbackPlan struct {
	s           *supervisor
	journal     rollbackJournal
	journalPath string
	old         *app
	running     bool
	dataDir     string
	hasData     bool
	stagedDB    string
	appMoved    bool
	dataMoved   bool
}

// stageRollback validates the snapshot request, stages its halves and writes the recovery journal.
func (s *supervisor) stageRollback(w http.ResponseWriter, name, stagedApp, snapshot string) (*rollbackPlan, bool) {
	if s.backups == nil || s.storage == nil {
		writeError(w, http.StatusServiceUnavailable, "coordinated rollback needs storage and backups")
		return nil, false
	}
	if pendingRollback(s.cfg.stateDir, name) {
		writeError(w, http.StatusConflict, "an earlier rollback is pending recovery; restart the supervisor")
		return nil, false
	}

	dataDir, hasData, stagedDB, err := s.backups.stageSnapshot(name, snapshot, true)
	if err != nil {
		writeBackupError(w, err)
		return nil, false
	}
	if !hasData && stagedDB == "" {
		removeIfExists(dataDir)
		removeIfExists(stagedDB)
		writeError(w, http.StatusBadRequest, "snapshot has no database or data archive")
		return nil, false
	}

	old := s.lookup(name)
	if old == nil {
		removeIfExists(dataDir)
		removeIfExists(stagedDB)
		writeError(w, http.StatusNotFound, "app not found")
		return nil, false
	}
	old.mu.Lock()
	running := old.cmd != nil && !old.exited
	old.mu.Unlock()

	liveApp := filepath.Join(s.cfg.appsDir, name)
	liveData := filepath.Join(s.cfg.stateDir, name)
	liveDB := s.storage.databasePath(name)
	suffix := fmt.Sprintf(".rollback-%d", time.Now().UnixNano())
	journal := rollbackJournal{
		Name:       name,
		Snapshot:   snapshot,
		AppDir:     liveApp,
		AppBackup:  liveApp + suffix,
		DataDir:    liveData,
		DataBackup: liveData + suffix,
		DB:         liveDB,
		DBBackup:   liveDB + suffix,
		StagedApp:  stagedApp,
		StagedData: dataDir,
		StagedDB:   stagedDB,
	}
	journalPath, err := writeRollbackJournal(s.cfg.stateDir, journal)
	if err != nil {
		removeIfExists(dataDir)
		removeIfExists(stagedDB)
		writeError(w, http.StatusInternalServerError, "write rollback journal: "+err.Error())
		return nil, false
	}
	return &rollbackPlan{
		s:           s,
		journal:     journal,
		journalPath: journalPath,
		old:         old,
		running:     running,
		dataDir:     dataDir,
		hasData:     hasData,
		stagedDB:    stagedDB,
	}, true
}

// installCode moves the live app aside and puts the staged bundle in its place.
func (p *rollbackPlan) installCode(w http.ResponseWriter) bool {
	var err error
	p.appMoved, err = moveAside(p.journal.AppDir, p.journal.AppBackup)
	if err == nil {
		err = os.Rename(p.journal.StagedApp, p.journal.AppDir)
	}
	if err != nil {
		_ = restoreAside(p.journal.AppDir, p.journal.AppBackup, p.appMoved)
		p.s.failRollback(w, p.journalPath, p.journal, p.old, p.running, fmt.Errorf("install code: %w", err))
		return false
	}
	return true
}

// installData replaces the live /data directory with the staged snapshot data.
func (p *rollbackPlan) installData(w http.ResponseWriter) bool {
	if !p.hasData {
		return true
	}
	var err error
	p.dataMoved, err = moveAside(p.journal.DataDir, p.journal.DataBackup)
	if err == nil {
		err = os.Rename(p.dataDir, p.journal.DataDir)
	}
	if err != nil {
		_ = restoreAside(p.journal.DataDir, p.journal.DataBackup, p.dataMoved)
		_ = restoreAside(p.journal.AppDir, p.journal.AppBackup, p.appMoved)
		p.s.failRollback(w, p.journalPath, p.journal, p.old, p.running, fmt.Errorf("install data: %w", err))
		return false
	}
	return true
}

// installDatabase swaps the staged database in while storage users are blocked from opening it.
func (p *rollbackPlan) installDatabase(w http.ResponseWriter) bool {
	if p.stagedDB == "" {
		return true
	}
	s := p.s
	// Block database viewers (and any other storage user) from opening the file while it is swapped.
	s.storage.opMu.Lock()
	s.storage.drop(p.journal.Name)
	removeSQLiteSidecars(p.journal.DB)
	swapErr := func() error {
		if err := os.MkdirAll(filepath.Dir(p.journal.DB), 0o755); err != nil {
			return fmt.Errorf("create storage directory: %w", err)
		}
		if _, err := moveAside(p.journal.DB, p.journal.DBBackup); err != nil {
			return err
		}
		return os.Rename(p.stagedDB, p.journal.DB)
	}()
	if swapErr != nil {
		_ = restoreDB(p.journal.DB, p.journal.DBBackup)
	}
	s.storage.opMu.Unlock()
	if swapErr != nil {
		_ = restoreAside(p.journal.DataDir, p.journal.DataBackup, p.dataMoved)
		_ = restoreAside(p.journal.AppDir, p.journal.AppBackup, p.appMoved)
		s.failRollback(w, p.journalPath, p.journal, p.old, p.running, fmt.Errorf("install database: %w", swapErr))
		return false
	}
	return true
}

// start launches the installed version, compensating every half if it does not come up.
func (p *rollbackPlan) start(w http.ResponseWriter, name string, cfg appConfig) *app {
	next, err := p.s.spawn(name, p.journal.AppDir, cfg)
	if err != nil {
		_ = restoreDB(p.journal.DB, p.journal.DBBackup)
		_ = restoreAside(p.journal.DataDir, p.journal.DataBackup, p.dataMoved)
		_ = restoreAside(p.journal.AppDir, p.journal.AppBackup, p.appMoved)
		p.s.failRollback(w, p.journalPath, p.journal, p.old, p.running, fmt.Errorf("new version did not start: %w", err))
		return nil
	}
	return next
}

// rollbackAtomic installs staged code and a snapshot together. The app is stopped for the swap, then the new version starts; any failure restores the previous code, data, database and process.
func (s *supervisor) rollbackAtomic(w http.ResponseWriter, ctx context.Context, name, stagedApp string, cfg appConfig, snapshot string) *app {
	plan, ok := s.stageRollback(w, name, stagedApp, snapshot)
	if !ok {
		return nil
	}
	defer removeIfExists(plan.dataDir)
	defer removeIfExists(plan.stagedDB)

	// No snapshot may start between the swap and the new app's first request.
	s.backups.mu.Lock()
	defer s.backups.mu.Unlock()

	if plan.running {
		s.drainAndStop(plan.old, "rollback")
	}
	if !plan.installCode(w) || !plan.installData(w) || !plan.installDatabase(w) {
		return nil
	}
	next := plan.start(w, name, cfg)
	if next == nil {
		return nil
	}

	next.dir = plan.journal.AppDir
	s.register(next)
	s.recordDeployment(name, next.dir)
	if err := s.backups.markCurrentLocked(name, snapshot); err != nil {
		log.Printf("rollback %s: mark snapshot current: %v", name, err)
	}

	journal := plan.journal
	journal.Committed = true
	if err := saveRollbackJournal(plan.journalPath, journal); err != nil {
		log.Printf("rollback %s: commit journal: %v", name, err)
	}
	removeIfExists(journal.AppBackup)
	removeIfExists(journal.DataBackup)
	removeIfExists(journal.DBBackup)
	_ = os.Remove(plan.journalPath)
	if plan.old.logs != nil {
		plan.old.logs.close()
	}
	log.Printf("%s %s at %s with snapshot %s", "rolled back", name, cfg.Host, snapshot)
	return next
}
