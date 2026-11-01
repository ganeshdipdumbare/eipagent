# hello_mcp

hello_mcp exposes a remote tool API as tools. At load it GETs <url>/tools ([{name, description, schema}]) and registers each as hello_mcp__<name>; calls POST <url>/tools/<name> with the JSON args. For real MCP servers use the built-in mcp_client plugin; this template shows how to adapt any HTTP tool API the same way.

A plugin made with `eip plugin init` to turn a remote HTTP tool API into tools.

```bash
eip plugin validate .
eip plugin build -key ~/.eip/signing.key .
```
