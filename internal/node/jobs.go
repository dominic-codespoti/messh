package node

import (
	"messh/internal/approval"
	"messh/internal/jobs"
	"messh/internal/provider"
)

// startJobs registers the job runner: approved programs run here as
// background jobs. It is closed (running jobs killed) when the node stops. A
// failure only costs jobs, never the node.
func (n *Node) startJobs() {
	j, err := jobs.New(n.ctx, jobs.Options{Paths: n.paths, Log: n.log.With("component", "jobs"), RestoreApproval: func(j jobs.Job) (provider.Ticket, error) {
		return n.approvals.Submit(approval.Request{Caller: provider.Caller{DeviceID: j.Owner.DeviceID, DeviceName: j.Owner.DeviceName, Agent: j.Owner.Agent}, Tool: "job_submit", Class: provider.ClassExec, Approval: jobs.ApprovalForJob(j)}), nil
	}})
	if err != nil {
		n.log.Warn("jobs unavailable", "error", err)
		return
	}
	n.jobs = j
	n.register(j)
	// shutdown waits for goRun goroutines after the servers stop, so running
	// jobs are gone by the time Done closes.
	n.goRun(func() {
		<-n.ctx.Done()
		j.Close()
	})
}
