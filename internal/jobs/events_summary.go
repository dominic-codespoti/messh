package jobs

import (
	"messh/internal/files"
	"strings"
)

func summaryOf(j *Job) Summary {
	return Summary{JobID: j.ID, Label: j.Label, State: j.State, Reason: j.Reason, Command: truncate(strings.Join(j.Argv(), " "), 200), Workspace: files.RootWorkspaces + "/" + j.Workspace, Submitted: j.Submitted, Finished: j.Finished, ExitCode: j.ExitCode}
}
