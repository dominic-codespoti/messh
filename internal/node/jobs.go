package node

import (
	"messh/internal/jobs"
)

// startJobs registers the job runner: approved programs run here as
// background jobs. It is closed (running jobs killed) when the node stops. A
// failure only costs jobs, never the node.
func (n *Node) startJobs() {
	j, err := jobs.New(n.ctx, jobs.Options{Paths: n.paths, Log: n.log.With("component", "jobs")})
	if err != nil {
		n.log.Warn("jobs unavailable", "error", err)
		return
	}
	n.register(j)
	// shutdown waits for goRun goroutines after the servers stop, so running
	// jobs are gone by the time Done closes.
	n.goRun(func() {
		<-n.ctx.Done()
		j.Close()
	})
}
