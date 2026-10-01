package jobs

// SleepInhibitor keeps the device awake while jobs run. Hold and Release are
// called by the provider as the running-job count goes 0→1 and 1→0; Hold is
// called again when a job starts while an earlier Hold failed or lapsed.
type SleepInhibitor interface {
	Hold() error
	Release()
}

// inhibitStatus is implemented by inhibitors whose hold can end on its own
// (Linux: the systemd-inhibit child exits). Status is "held", "not held", or
// "unavailable: <reason>".
type inhibitStatus interface {
	Status() string
}

// holdSleepLocked takes the sleep inhibitor when the first job starts, and
// retries when a later job starts while the hold failed or lapsed. A failure
// never fails the job; each new failure is logged once and then shown in job
// notes and job_resources.
func (p *Provider) holdSleepLocked() {
	if p.running > 1 && p.sleepStatusLocked() == "held" {
		return
	}
	if err := p.inh.Hold(); err != nil {
		if msg := err.Error(); msg != p.sleepErr {
			p.sleepErr = msg
			p.log.Warn("cannot keep the device awake while jobs run", "error", err)
		}
		return
	}
	p.sleepErr = ""
}

// sleepStatusLocked reports the sleep inhibitor: "held", "not held", or
// "unavailable: <reason>".
func (p *Provider) sleepStatusLocked() string {
	if s, ok := p.inh.(inhibitStatus); ok {
		return s.Status()
	}
	switch {
	case p.sleepErr != "":
		return "unavailable: " + p.sleepErr
	case p.running > 0:
		return "held"
	}
	return "not held"
}
