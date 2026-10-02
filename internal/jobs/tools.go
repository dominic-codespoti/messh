package jobs

import (
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"messh/internal/provider"
)

const (
	toolSubmit    = "job_submit"
	toolStatus    = "job_status"
	toolWait      = "job_wait"
	toolLogs      = "job_logs"
	toolCancel    = "job_cancel"
	toolList      = "job_list"
	toolDelete    = "job_delete"
	toolResources = "job_resources"

	maxWaitSeconds = 300
)

const retentionNote = " Nothing is deleted automatically: a job's output files and logs stay on the device until you call job_delete."

var (
	idProp = `"job_id":{"type":"string","description":"Id returned by job_submit."}`

	submitSchema = json.RawMessage(`{
  "type":"object",
  "properties":{
    "request_id":{"type":"string","minLength":1,"maxLength":128},
    "recovery":{"type":"object","properties":{"checkpoint":{"type":"string"},"args":{"type":"array","items":{"type":"string"}}},"required":["checkpoint","args"],"additionalProperties":false},
    "command":{"type":"string","description":"Program to run, resolved on this device: a bare name found on PATH or an absolute path."},
    "args":{"type":"array","items":{"type":"string"},"description":"Arguments, one array element each; no quoting needed. Leave empty with shell:true."},
    "cwd":{"type":"string","description":"Directory to start in, relative to the job workspace (forward slashes, must stay inside it). Default: the workspace root."},
    "env":{"type":"object","additionalProperties":{"type":"string"},"description":"Environment variables to add or override on top of the user's environment on this device."},
    "shell":{"type":"boolean","description":"Run command as a command line through cmd /C (Windows) or sh -c (Linux). Prefer args; shell jobs need broader approval."},
    "inputs":{"type":"array","items":{"type":"string"},"description":"Files already on this device to snapshot into the workspace first, as refs: ws/<workspace>/path or artifacts/<source>/file. A ws/<other>/dir/file input appears at dir/file inside the workspace. Bring files from other devices with the file copy tool first. The owner approves the exact content (SHA-256)."},
    "workspace":{"type":"string","description":"Name of the workspace directory (ws/<workspace>) the job runs in. Default: a new one named after the job id. Reuse a name to chain jobs on the same files, or to run in a workspace the file copy tool has filled."},
    "resources":{"type":"object","description":"What the job needs. The job waits in a FIFO queue until all of it is free.","properties":{
      "gpus":{"type":"array","items":{"type":"integer","minimum":0},"description":"GPU indexes to claim exclusively (see job_resources). CUDA_VISIBLE_DEVICES is set to them."},
      "vram_mb":{"type":"integer","minimum":0,"description":"VRAM needed, in MB. Checked against free VRAM when the job would start."},
      "mem_mb":{"type":"integer","minimum":0,"description":"RAM needed, in MB. Checked against available RAM and enforced as a hard limit where the OS allows."},
      "cpus":{"type":"integer","minimum":0,"description":"CPU threads to reserve in the scheduler."}
    },"additionalProperties":false},
    "timeout_seconds":{"type":"integer","minimum":0,"description":"Kill the job (and everything it started) after this many seconds. 0 or omitted: no limit."},
    "label":{"type":"string","description":"Short note for you; shown in job_list."}
  },
  "required":["command"],
  "additionalProperties":false
}`)

	statusSchema = json.RawMessage(`{
  "type":"object",
  "properties":{` + idProp + `,"tail_lines":{"type":"integer","minimum":0,"maximum":200,"description":"How many trailing log lines to include per stream (default 20)."}},
  "required":["job_id"],
  "additionalProperties":false
}`)

	waitSchema = json.RawMessage(`{
  "type":"object",
  "properties":{` + idProp + `,"timeout_seconds":{"type":"integer","minimum":1,"maximum":300,"description":"Longest to block (default 30, max 300). Returns the current status when it elapses."}},
  "required":["job_id"],
  "additionalProperties":false
}`)

	logsSchema = json.RawMessage(`{
  "type":"object",
  "properties":{` + idProp + `,
    "stream":{"type":"string","enum":["stdout","stderr","both"],"description":"Default both."},
    "tail_lines":{"type":"integer","minimum":1,"maximum":2000,"description":"Return the last N lines (default 100). Ignored when offset is given."},
    "offset":{"type":"integer","minimum":0,"description":"Read forward from this byte position of one stream (use next_offset from the previous call to follow a running job). Needs stream stdout or stderr."},
    "max_bytes":{"type":"integer","minimum":1,"maximum":1048576,"description":"With offset: most bytes to return (default 65536)."}
  },
  "required":["job_id"],
  "additionalProperties":false
}`)

	idOnlySchema = json.RawMessage(`{
  "type":"object",
  "properties":{` + idProp + `},
  "required":["job_id"],
  "additionalProperties":false
}`)

	listSchema = json.RawMessage(`{
  "type":"object",
  "properties":{"state":{"type":"string","enum":["submitted","awaiting_approval","queued","running","succeeded","failed","cancelled","interrupted"],"description":"Only jobs in this state."}},
  "additionalProperties":false
}`)

	emptySchema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
)

func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{ReadOnlyHint: true, IdempotentHint: true, Title: title}
}

func (p *Provider) Tools() []provider.Tool {
	t := true
	f := false
	return []provider.Tool{
		{Class: provider.ClassExec, Def: &mcp.Tool{
			Name: toolSubmit,
			Description: "Run a program on this device as a background job (e.g. a GPU job on the desktop). " +
				"The device's owner must approve every submission on that device; the call returns at once with a job_id " +
				"and the job starts by itself when approved and its resources are free. Follow it with job_wait / job_status / job_logs. " +
				"The job runs as the logged-in user in a workspace directory ws/<workspace>; files it writes there are listed " +
				"as refs (ws/<workspace>/file) in job_status and can be fetched with the file copy tool. " +
				"Cancelling, timing out or stopping the node kills the job's whole process tree.",
			InputSchema: submitSchema,
			Annotations: &mcp.ToolAnnotations{Title: "Submit job", DestructiveHint: &t, OpenWorldHint: &f},
		}},
		{Class: provider.ClassInfo, Def: &mcp.Tool{
			Name: toolStatus,
			Description: "Show a job: state (submitted, awaiting_approval, queued, running, succeeded, failed, cancelled, interrupted), reason, " +
				"exit code, timestamps, the resolved command, queue position, output files (refs with sizes) and the last log lines." + retentionNote,
			InputSchema: statusSchema,
			Annotations: readOnly("Job status"),
		}},
		{Class: provider.ClassInfo, Def: &mcp.Tool{
			Name: toolWait,
			Description: "Block until the job reaches a final state (succeeded, failed, cancelled, interrupted) or timeout_seconds (max 300) elapse, " +
				"then return the same data as job_status. A job waiting for approval stays waiting; call again.",
			InputSchema: waitSchema,
			Annotations: readOnly("Wait for job"),
		}},
		{Class: provider.ClassInfo, Def: &mcp.Tool{
			Name: toolLogs,
			Description: "Read a job's stdout/stderr: the last N lines, or forward from a byte offset to follow a running job. " +
				"Very long output keeps its first 4 MiB and the most recent part; earlier_output_omitted / skipped_bytes say when text was rolled away.",
			InputSchema: logsSchema,
			Annotations: readOnly("Job logs"),
		}},
		{Class: provider.ClassInfo, Def: &mcp.Tool{
			Name: toolCancel,
			Description: "Cancel a job: withdraws a pending approval, removes it from the queue, or kills a running job and every process it started. " +
				"Only the agent that submitted the job can cancel it.",
			InputSchema: idOnlySchema,
			Annotations: &mcp.ToolAnnotations{Title: "Cancel job", DestructiveHint: &f, IdempotentHint: true},
		}},
		{Class: provider.ClassInfo, Def: &mcp.Tool{
			Name:        toolList,
			Description: "List your jobs on this device, newest first, optionally only one state." + retentionNote,
			InputSchema: listSchema,
			Annotations: readOnly("List jobs"),
		}},
		{Class: provider.ClassInfo, Def: &mcp.Tool{
			Name: toolDelete,
			Description: "Delete a finished job: its record and logs, and its workspace ws/<workspace> with every output file unless another job " +
				"still uses that workspace. Fetch outputs you need first. Refused for jobs that have not finished.",
			InputSchema: idOnlySchema,
			Annotations: &mcp.ToolAnnotations{Title: "Delete job", DestructiveHint: &t, IdempotentHint: true},
		}},
		{Class: provider.ClassInfo, Def: &mcp.Tool{
			Name: toolResources,
			Description: "What this device can offer jobs right now: CPU threads, RAM, each GPU with VRAM total and free and whether a job holds it, " +
				"and the job queue load. Use it to pick resources.gpus / vram_mb before job_submit.",
			InputSchema: emptySchema,
			Annotations: readOnly("Job resources"),
		}},
	}
}
