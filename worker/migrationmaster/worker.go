// Copyright 2016 Canonical Ltd.
// Licensed under the AGPLv3, see LICENCE file for details.

package migrationmaster

import (
	"fmt"
	"strings"
	"time"

	"github.com/juju/errors"
	"github.com/juju/loggo"
	"github.com/juju/utils/clock"
	"gopkg.in/juju/names.v2"

	"github.com/juju/juju/api"
	"github.com/juju/juju/api/migrationtarget"
	"github.com/juju/juju/apiserver/params"
	coremigration "github.com/juju/juju/core/migration"
	"github.com/juju/juju/migration"
	"github.com/juju/juju/watcher"
	"github.com/juju/juju/worker/catacomb"
	"github.com/juju/juju/worker/dependency"
	"github.com/juju/juju/worker/fortress"
)

var (
	logger = loggo.GetLogger("juju.worker.migrationmaster")

	// ErrDoneForNow indicates a temporary issue was encountered and
	// that the worker should restart and retry.
	ErrDoneForNow = errors.New("done for now")
)

const (
	// maxMinionWait is the maximum time that the migrationmaster will
	// wait for minions to report back regarding a given migration
	// phase.
	maxMinionWait = 15 * time.Minute

	// minionWaitLogInterval is the time between progress update
	// messages, while the migrationmaster is waiting for reports from
	// minions.
	minionWaitLogInterval = 30 * time.Second
)

// Facade exposes controller functionality to a Worker.
type Facade interface {
	// Watch returns a watcher which reports when a migration is
	// active for the model associated with the API connection.
	Watch() (watcher.NotifyWatcher, error)

	// GetMigrationStatus returns the details and progress of the
	// latest model migration.
	GetMigrationStatus() (coremigration.MigrationStatus, error)

	// SetPhase updates the phase of the currently active model
	// migration.
	SetPhase(coremigration.Phase) error

	// Export returns a serialized representation of the model
	// associated with the API connection.
	Export() (coremigration.SerializedModel, error)

	// Reap removes all documents of the model associated with the API
	// connection.
	Reap() error

	// WatchMinionReports returns a watcher which reports when a migration
	// minion has made a report for the current migration phase.
	WatchMinionReports() (watcher.NotifyWatcher, error)

	// GetMinionReports returns details of the reports made by migration
	// minions to the controller for the current migration phase.
	GetMinionReports() (coremigration.MinionReports, error)
}

// Config defines the operation of a Worker.
type Config struct {
	Facade          Facade
	Guard           fortress.Guard
	APIOpen         func(*api.Info, api.DialOpts) (api.Connection, error)
	UploadBinaries  func(migration.UploadBinariesConfig) error
	CharmDownloader migration.CharmDownloader
	ToolsDownloader migration.ToolsDownloader
	Clock           clock.Clock
}

// Validate returns an error if config cannot drive a Worker.
func (config Config) Validate() error {
	if config.Facade == nil {
		return errors.NotValidf("nil Facade")
	}
	if config.Guard == nil {
		return errors.NotValidf("nil Guard")
	}
	if config.APIOpen == nil {
		return errors.NotValidf("nil APIOpen")
	}
	if config.UploadBinaries == nil {
		return errors.NotValidf("nil UploadBinaries")
	}
	if config.CharmDownloader == nil {
		return errors.NotValidf("nil CharmDownloader")
	}
	if config.ToolsDownloader == nil {
		return errors.NotValidf("nil ToolsDownloader")
	}
	if config.Clock == nil {
		return errors.NotValidf("nil Clock")
	}
	return nil
}

// New returns a Worker backed by config, or an error.
func New(config Config) (*Worker, error) {
	if err := config.Validate(); err != nil {
		return nil, errors.Trace(err)
	}
	w := &Worker{
		config: config,
	}
	err := catacomb.Invoke(catacomb.Plan{
		Site: &w.catacomb,
		Work: w.run,
	})
	if err != nil {
		return nil, errors.Trace(err)
	}
	return w, nil
}

// Worker waits until a migration is active and its configured
// Fortress is locked down, and then orchestrates a model migration.
type Worker struct {
	catacomb catacomb.Catacomb
	config   Config
}

// Kill implements worker.Worker.
func (w *Worker) Kill() {
	w.catacomb.Kill(nil)
}

// Wait implements worker.Worker.
func (w *Worker) Wait() error {
	return w.catacomb.Wait()
}

func (w *Worker) run() error {
	status, err := w.waitForActiveMigration()
	if err != nil {
		return errors.Trace(err)
	}

	err = w.config.Guard.Lockdown(w.catacomb.Dying())
	if errors.Cause(err) == fortress.ErrAborted {
		return w.catacomb.ErrDying()
	} else if err != nil {
		return errors.Trace(err)
	}

	// TODO(mjs) - log messages should indicate the model name and
	// UUID. Independent logger per migration instance?

	phase := status.Phase
	for {
		var err error
		switch phase {
		case coremigration.QUIESCE:
			phase, err = w.doQUIESCE()
		case coremigration.READONLY:
			phase, err = w.doREADONLY()
		case coremigration.PRECHECK:
			phase, err = w.doPRECHECK()
		case coremigration.IMPORT:
			phase, err = w.doIMPORT(status.TargetInfo, status.ModelUUID)
		case coremigration.VALIDATION:
			phase, err = w.doVALIDATION(status.TargetInfo, status.ModelUUID)
		case coremigration.SUCCESS:
			phase, err = w.doSUCCESS(status)
		case coremigration.LOGTRANSFER:
			phase, err = w.doLOGTRANSFER()
		case coremigration.REAP:
			phase, err = w.doREAP()
		case coremigration.ABORT:
			phase, err = w.doABORT(status.TargetInfo, status.ModelUUID)
		default:
			return errors.Errorf("unknown phase: %v [%d]", phase.String(), phase)
		}

		if err != nil {
			// A phase handler should only return an error if the
			// migration master should exit. In the face of other
			// errors the handler should log the problem and then
			// return the appropriate error phase to transition to -
			// i.e. ABORT or REAPFAILED)
			return errors.Trace(err)
		}

		if w.killed() {
			return w.catacomb.ErrDying()
		}

		logger.Infof("setting migration phase to %s", phase)
		if err := w.config.Facade.SetPhase(phase); err != nil {
			return errors.Annotate(err, "failed to set phase")
		}
		status.Phase = phase

		if modelHasMigrated(phase) {
			// TODO(mjs) - use manifold Filter so that the dep engine
			// error types aren't required here.
			return dependency.ErrUninstall
		} else if phase.IsTerminal() {
			// Some other terminal phase, exit and try again.
			return ErrDoneForNow
		}
	}
}

func (w *Worker) killed() bool {
	select {
	case <-w.catacomb.Dying():
		return true
	default:
		return false
	}
}

func (w *Worker) doQUIESCE() (coremigration.Phase, error) {
	// TODO(mjs) - Wait for all agents to report back.
	return coremigration.READONLY, nil
}

func (w *Worker) doREADONLY() (coremigration.Phase, error) {
	// TODO(mjs) - To be implemented.
	return coremigration.PRECHECK, nil
}

func (w *Worker) doPRECHECK() (coremigration.Phase, error) {
	// TODO(mjs) - To be implemented.
	return coremigration.IMPORT, nil
}

func (w *Worker) doIMPORT(targetInfo coremigration.TargetInfo, modelUUID string) (coremigration.Phase, error) {
	logger.Infof("exporting model")
	serialized, err := w.config.Facade.Export()
	if err != nil {
		logger.Errorf("model export failed: %v", err)
		return coremigration.ABORT, nil
	}

	logger.Infof("opening API connection to target controller")
	conn, err := w.openAPIConn(targetInfo)
	if err != nil {
		logger.Errorf("failed to connect to target controller: %v", err)
		return coremigration.ABORT, nil
	}
	defer conn.Close()

	logger.Infof("importing model into target controller")
	targetClient := migrationtarget.NewClient(conn)
	err = targetClient.Import(serialized.Bytes)
	if err != nil {
		logger.Errorf("failed to import model into target controller: %v", err)
		return coremigration.ABORT, nil
	}

	logger.Infof("opening API connection for target model")
	targetModelConn, err := w.openAPIConnForModel(targetInfo, modelUUID)
	if err != nil {
		logger.Errorf("failed to open connection to target model: %v", err)
		return coremigration.ABORT, nil
	}
	defer targetModelConn.Close()
	targetModelClient := targetModelConn.Client()

	logger.Infof("uploading binaries into target model")
	err = w.config.UploadBinaries(migration.UploadBinariesConfig{
		Charms:          serialized.Charms,
		CharmDownloader: w.config.CharmDownloader,
		CharmUploader:   targetModelClient,
		Tools:           serialized.Tools,
		ToolsDownloader: w.config.ToolsDownloader,
		ToolsUploader:   targetModelClient,
	})
	if err != nil {
		logger.Errorf("failed migration binaries: %v", err)
		return coremigration.ABORT, nil
	}

	return coremigration.VALIDATION, nil
}

func (w *Worker) doVALIDATION(targetInfo coremigration.TargetInfo, modelUUID string) (coremigration.Phase, error) {
	// TODO(mjs) - Wait for all agents to report back.

	// Once all agents have validated, activate the model.
	err := w.activateModel(targetInfo, modelUUID)
	if err != nil {
		return coremigration.ABORT, nil
	}
	return coremigration.SUCCESS, nil
}

func (w *Worker) activateModel(targetInfo coremigration.TargetInfo, modelUUID string) error {
	conn, err := w.openAPIConn(targetInfo)
	if err != nil {
		return errors.Trace(err)
	}
	defer conn.Close()

	targetClient := migrationtarget.NewClient(conn)
	err = targetClient.Activate(modelUUID)
	return errors.Trace(err)
}

func (w *Worker) doSUCCESS(status coremigration.MigrationStatus) (coremigration.Phase, error) {
	err := w.waitForMinions(status, waitForAll)
	switch errors.Cause(err) {
	case nil, errMinionReportFailed, errMinionReportTimeout:
		// There's no turning back from SUCCESS - any problems should
		// have been picked up in VALIDATION. After the minion wait in
		// the SUCCESS phase, the migration can only proceed to
		// LOGTRANSFER.
		return coremigration.LOGTRANSFER, nil
	default:
		return coremigration.SUCCESS, errors.Trace(err)
	}
}

func (w *Worker) doLOGTRANSFER() (coremigration.Phase, error) {
	// TODO(mjs) - To be implemented.
	return coremigration.REAP, nil
}

func (w *Worker) doREAP() (coremigration.Phase, error) {
	err := w.config.Facade.Reap()
	if err != nil {
		return coremigration.REAPFAILED, errors.Trace(err)
	}
	return coremigration.DONE, nil
}

func (w *Worker) doABORT(targetInfo coremigration.TargetInfo, modelUUID string) (coremigration.Phase, error) {
	if err := w.removeImportedModel(targetInfo, modelUUID); err != nil {
		// This isn't fatal. Removing the imported model is a best
		// efforts attempt.
		logger.Errorf("failed to reverse model import: %v", err)
	}
	return coremigration.ABORTDONE, nil
}

func (w *Worker) removeImportedModel(targetInfo coremigration.TargetInfo, modelUUID string) error {
	conn, err := w.openAPIConn(targetInfo)
	if err != nil {
		return errors.Trace(err)
	}
	defer conn.Close()

	targetClient := migrationtarget.NewClient(conn)
	err = targetClient.Abort(modelUUID)
	return errors.Trace(err)
}

func (w *Worker) waitForActiveMigration() (coremigration.MigrationStatus, error) {
	var empty coremigration.MigrationStatus

	watcher, err := w.config.Facade.Watch()
	if err != nil {
		return empty, errors.Annotate(err, "watching for migration")
	}
	if err := w.catacomb.Add(watcher); err != nil {
		return empty, errors.Trace(err)
	}
	defer watcher.Kill()

	for {
		select {
		case <-w.catacomb.Dying():
			return empty, w.catacomb.ErrDying()
		case <-watcher.Changes():
		}
		status, err := w.config.Facade.GetMigrationStatus()
		switch {
		case params.IsCodeNotFound(err):
			if err := w.config.Guard.Unlock(); err != nil {
				return empty, errors.Trace(err)
			}
			continue
		case err != nil:
			return empty, errors.Annotate(err, "retrieving migration status")
		}
		if modelHasMigrated(status.Phase) {
			return empty, dependency.ErrUninstall
		}
		if !status.Phase.IsTerminal() {
			return status, nil
		}
	}
}

// Possible values for waitForMinion's waitPolicy argument.
const failFast = false  // Stop waiting at first minion failure report
const waitForAll = true // Wait for all minion reports to arrive (or timeout)

var errMinionReportTimeout = errors.New("timed out waiting for all minions to report")
var errMinionReportFailed = errors.New("one or more minions failed a migration phase")

func (w *Worker) waitForMinions(status coremigration.MigrationStatus, waitPolicy bool) error {
	clk := w.config.Clock
	maxWait := maxMinionWait - clk.Now().Sub(status.PhaseChangedTime)
	timeout := clk.After(maxWait)
	logger.Infof("waiting for minions to report back for migration phase %s (will wait up to %s)",
		status.Phase, truncDuration(maxWait))

	watch, err := w.config.Facade.WatchMinionReports()
	if err != nil {
		return errors.Trace(err)
	}
	if err := w.catacomb.Add(watch); err != nil {
		return errors.Trace(err)
	}

	logProgress := clk.After(minionWaitLogInterval)

	var reports coremigration.MinionReports
	for {
		select {
		case <-w.catacomb.Dying():
			return w.catacomb.ErrDying()

		case <-timeout:
			logger.Errorf(formatMinionTimeout(reports, status))
			return errors.Trace(errMinionReportTimeout)

		case <-watch.Changes():
			var err error
			reports, err = w.config.Facade.GetMinionReports()
			if err != nil {
				return errors.Trace(err)
			}
			if err := validateMinionReports(reports, status); err != nil {
				return errors.Trace(err)
			}
			failures := len(reports.FailedMachines) + len(reports.FailedUnits)
			if failures > 0 {
				logger.Errorf(formatMinionFailure(reports))
				if waitPolicy == failFast {
					return errors.Trace(errMinionReportFailed)
				}
			}
			if reports.UnknownCount == 0 {
				logger.Infof(formatMinionWaitDone(reports))
				if failures > 0 {
					return errors.Trace(errMinionReportFailed)
				}
				return nil
			}

		case <-logProgress:
			logger.Infof(formatMinionWaitUpdate(reports, status))
			logProgress = clk.After(minionWaitLogInterval)
		}
	}
}

func truncDuration(d time.Duration) time.Duration {
	return (d / time.Second) * time.Second
}

func validateMinionReports(reports coremigration.MinionReports, status coremigration.MigrationStatus) error {
	// TODO(mjs) the migration id should be part of the status response.
	migId := fmt.Sprintf("%s:%d", status.ModelUUID, status.Attempt)
	if reports.MigrationId != migId {
		return errors.Errorf("unexpected migration id in minion reports, got %v, expected %v",
			reports.MigrationId, migId)
	}
	if reports.Phase != status.Phase {
		return errors.Errorf("minion reports phase (%s) does not match migration phase (%s)",
			reports.Phase, status.Phase)
	}
	return nil
}

func formatMinionTimeout(reports coremigration.MinionReports, status coremigration.MigrationStatus) string {
	if reports.IsZero() {
		return fmt.Sprintf("no agents reported in time for migration phase %s", status.Phase)
	}

	msg := "%s agents failed to report in time for migration phase %s including:"
	if len(reports.SomeUnknownMachines) > 0 {
		msg += fmt.Sprintf("machines: %s;", strings.Join(reports.SomeUnknownMachines, ", "))
	}
	if len(reports.SomeUnknownUnits) > 0 {
		msg += fmt.Sprintf(" units: %s", strings.Join(reports.SomeUnknownUnits, ", "))
	}
	return msg
}

func formatMinionFailure(reports coremigration.MinionReports) string {
	msg := fmt.Sprintf("some agents failed %s: ", reports.Phase)
	if len(reports.FailedMachines) > 0 {
		msg += fmt.Sprintf("failed machines: %s; ", strings.Join(reports.FailedMachines, ", "))
	}
	if len(reports.FailedUnits) > 0 {
		msg += fmt.Sprintf("failed units: %s", strings.Join(reports.FailedUnits, ", "))
	}
	return msg
}

func formatMinionWaitUpdate(reports coremigration.MinionReports, status coremigration.MigrationStatus) string {
	if reports.IsZero() {
		return fmt.Sprintf("no reports from minions yet for %s", status.Phase)
	}

	msg := fmt.Sprintf("waiting for minions to report for %s: %d succeeded, %d still to report",
		reports.Phase, reports.SuccessCount, reports.UnknownCount)
	failed := len(reports.FailedMachines) + len(reports.FailedUnits)
	if failed > 0 {
		msg += fmt.Sprintf(", %d failed", failed)
	}
	return msg
}

func formatMinionWaitDone(reports coremigration.MinionReports) string {
	return fmt.Sprintf("completed waiting for minions to report for %s: %d succeeded, %d failed",
		reports.Phase, reports.SuccessCount, len(reports.FailedMachines)+len(reports.FailedUnits))
}

func (w *Worker) openAPIConn(targetInfo coremigration.TargetInfo) (api.Connection, error) {
	return w.openAPIConnForModel(targetInfo, "")
}

func (w *Worker) openAPIConnForModel(targetInfo coremigration.TargetInfo, modelUUID string) (api.Connection, error) {
	apiInfo := &api.Info{
		Addrs:    targetInfo.Addrs,
		CACert:   targetInfo.CACert,
		Tag:      targetInfo.AuthTag,
		Password: targetInfo.Password,
		ModelTag: names.NewModelTag(modelUUID),
	}
	// Use zero DialOpts (no retries) because the worker must stay
	// responsive to Kill requests. We don't want it to be blocked by
	// a long set of retry attempts.
	return w.config.APIOpen(apiInfo, api.DialOpts{})
}

func modelHasMigrated(phase coremigration.Phase) bool {
	return phase == coremigration.DONE || phase == coremigration.REAPFAILED
}
