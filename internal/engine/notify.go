package engine

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/jhuancz/cronkite/internal/config"
	"github.com/jhuancz/cronkite/internal/notify"
	"github.com/jhuancz/cronkite/internal/report"
)

// finishRun ends a run: it writes the report, delivers the alert the outcome
// calls for, and rewrites the report when delivery added a note.
//
// The order is the contract. A message names the report path, and an operator
// who reads the alert immediately has to find the file it points at, so nothing
// is sent before the report exists. A delivery problem then belongs in that same
// report, which is why a second write happens only when there is something new
// to say.
func finishRun(ctx context.Context, cfg *config.Config, rep *report.Report, sender notify.Notifier) error {
	if err := writeReports(cfg, rep); err != nil {
		return err
	}
	if !notifyRun(ctx, cfg, rep, sender) {
		return nil
	}
	return writeReports(cfg, rep)
}

// notifyRun delivers the alert for a finished run and reports whether it
// recorded a note that belongs in the report.
//
// A notifier failure never changes the run: the exit code and the status
// describe the maintenance, not the messenger. A Telegram outage, a wrong chat
// id or an expired token is exactly the kind of problem that must not hide the
// cleanup result behind itself.
func notifyRun(ctx context.Context, cfg *config.Config, rep *report.Report, sender notify.Notifier) bool {
	if !cfg.Notify.Enabled {
		return false
	}
	// A rehearsal never sends: an operator running a dry run must not learn that
	// their bot posted to a team channel.
	if rep.Mode != report.ModeApply {
		return false
	}
	if !notify.NewFilter(cfg.Notify.On).Allows(rep.Status) {
		return false
	}

	throttle := notify.NewThrottle(notify.StatePath(cfg.Run.StateDir), cfg.NotifyThrottle(), nil)
	if throttle.Suppressed(rep.Status) {
		rep.Notes = append(rep.Notes, fmt.Sprintf(
			"notify: a repeat of status %s inside notify.throttle (%s) was suppressed; a worse outcome would have been sent",
			rep.Status, cfg.Notify.Throttle))
		return true
	}

	if sender == nil {
		sender = notify.NewTelegram(notify.TelegramOptions{
			ChatID:   cfg.Notify.Telegram.ChatID,
			TokenEnv: cfg.Notify.Telegram.TokenEnv,
		})
	}

	message := notify.Message{
		Status:      rep.Status,
		RunID:       rep.RunID,
		Host:        rep.Host.Hostname,
		Totals:      rep.Totals,
		FailedTasks: failedTaskNames(rep),
		ReportPath:  reportPath(cfg, rep),
	}
	if err := sender.Send(ctx, message); err != nil {
		rep.Notes = append(rep.Notes, "notify: "+err.Error())
		return true
	}
	if err := throttle.Record(rep.Status); err != nil {
		// The message arrived; only the memory of it did not. Say so, because the
		// next run will repeat the alert.
		rep.Notes = append(rep.Notes, "notify: delivered, but the throttle state could not be written: "+err.Error())
		return true
	}
	return false
}

// failedTaskNames lists the tasks whose report says something went wrong. A
// partial task belongs in the list: it reported failures, it just did not let
// them stop the rest of its work.
func failedTaskNames(rep *report.Report) []string {
	var names []string
	for _, task := range rep.Tasks {
		if task.Status == report.StatusFailed || task.Status == report.StatusPartial {
			names = append(names, task.Name)
		}
	}
	return names
}

// reportPath is the per-run report file, the path an alert names.
func reportPath(cfg *config.Config, rep *report.Report) string {
	return filepath.Join(cfg.Report.Dir, "run-"+rep.RunID+".json")
}
