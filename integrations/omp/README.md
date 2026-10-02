# messh with omp

This guide configures omp's MCP client to talk to the local messh node. On the device where omp runs, start its node in a terminal:

```sh
messh node
```

In a separate terminal on that device, register an explicit identity:

```sh
messh agent add omp
```

The command reports the local `mcp_url` and a `token_command`; it does not print the secret. Add this MCP server entry to `~/.omp/agent/mcp.json` (merge with existing `mcpServers` entries), replacing values with those from the command:

```json
{
  "mcpServers": {
    "messh": {
      "type": "http",
      "url": "http://127.0.0.1:7520/mcp",
      "headers": {
        "Authorization": "!messh agent token omp --bearer"
      }
    }
  }
}
```

In omp's configuration, the leading `!` runs the command and uses its output as the header. If the node uses a non-default state directory, include its `--state DIR` in the token command. Keep the endpoint local; never expose the node or token command output in a public config or log. Avoid naming this server `browser` or `playwright`, which omp may treat as built-in browser automation.

After reloading omp, use `mesh_nodes`, then `mesh_tools` and `mesh_call` for a read-only smoke call such as remote `node_info`. Do not submit a job merely to verify the connection.

For the CLI skill, select the adapter explicitly: `messh skill install --for omp`. For pairing, platform requirements, security limits, and jobs, see the [project README](../../README.md), [reference](../../docs/reference.md), and [job durability contract](../../docs/jobs.md). Approval is human consent, not an OS sandbox.

## Use a remote model

On the model-host device, register its OpenAI-compatible service:

```sh
messh service add model http://127.0.0.1:11434/v1 --kind openai
```

On the device running omp, ensure the named agent is registered, then request omp's explicit model configuration:

```sh
messh agent add assistant
messh llm config desktop model --for omp --agent assistant
```

Replace the service/device names and URL with your setup. Merge the generated provider under `providers:` in `~/.omp/agent/models.yml`. Its command-backed API key reads the registered agent token at request time; keep the command intact and do not replace it with a pasted secret. Select the provider/model in omp as the command output describes. See the [model proxy reference](../../docs/reference.md#use-a-model-on-another-device) for forwarded routes, approvals, and timeouts.
