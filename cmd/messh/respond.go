package main

import (
	"context"
	"errors"
	"flag"
	"strings"
	"time"

	"messh/internal/approval"
)

func init() {
	add(&Command{
		Name:    "respond",
		Args:    []Arg{{Name: "URL", Help: "the messh-approve:// URL from the approval toast button"}},
		Summary: "deliver an approval toast button's answer to the node",
		Help: "Windows runs this when a button on an approval toast is clicked: it is the OS protocol handler " +
			"for messh-approve:// URLs, not a command people or agents run directly. It has no control token; " +
			"the URL's one-time nonce is its only credential. It prints nothing on success. The registered " +
			"handler passes --notify, which shows one small message when the answer could not be delivered, so a " +
			"click never fails silently; without --notify it only returns the error. It never waits for input.",
		Flags: func(fs *flag.FlagSet) {
			fs.Bool("notify", false, "on failure, also show a message box (set by the registered protocol handler)")
		},
		Output:   `{"delivered": true}`,
		Hidden:   true,
		Mutates:  true,
		Waits:    "up to 15 seconds for the node to accept the answer",
		Examples: []string{"messh respond messh-approve://respond?port=7520&id=3fa85f64&nonce=0123456789abcdef0123456789abcdef&decision=once"},
		Run: func(c *Context) error {
			if err := respond(c.Args); err != nil {
				if c.Bool("notify") {
					showError("messh approval", friendlyRespondError(err))
				}
				return err
			}
			return c.Emit(map[string]bool{"delivered": true}, nil)
		},
	})
}

// respond handles a messh-approve:// URL from an approval toast. It has
// no control token; the URL's one-time nonce is its only credential. It
// prints nothing on success and shows one small message when the answer
// could not be delivered, so a click never fails silently.
func respond(args []string) error {
	if len(args) != 1 {
		return errors.New("usage: messh respond messh-approve://respond?...")
	}
	target, err := approval.ParseRespondURL(args[0])
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return target.Send(ctx)
}

func friendlyRespondError(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "no longer waiting"):
		return "This approval request is no longer waiting: it was answered somewhere else, timed out, or the messh node restarted.\n\nNothing was changed."
	case strings.Contains(msg, "not reachable"):
		return "Your answer could not be delivered: the messh node is not running (or restarted).\n\nNothing was approved or denied; the request is gone."
	}
	return "Your answer could not be delivered: " + msg + "\n\nNothing was approved or denied."
}
