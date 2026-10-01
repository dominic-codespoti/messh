package node

import (
	"messh/internal/catalog"
)

// startCatalog registers the local service catalogue: the services the owner
// listed in services.json, exposed as gated tools. It is closed when the node
// stops. A failure only costs the catalogue, never the node.
func (n *Node) startCatalog() {
	c, err := catalog.New(n.ctx, catalog.Options{Paths: n.paths, Log: n.log})
	if err != nil {
		n.log.Warn("service catalogue unavailable", "error", err)
		return
	}
	n.catalog = c
	n.register(c)
	// shutdown waits for goRun goroutines after the servers stop; closing
	// here releases the upstream MCP sessions at that point.
	n.goRun(func() {
		<-n.ctx.Done()
		c.Close()
	})
}
