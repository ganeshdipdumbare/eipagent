# Coming from ADK Go

kiln keeps the ADK shape — an agent value, typed tools, a conversation — and
adds the security layer (broker, policy, audit) that ADK leaves to you.
The runnable version is [`examples/adk-parity`](../examples/adk-parity/main.go);
it needs no API key.

| ADK Go | kiln |
|---|---|
| `gemini.NewModel(ctx, "…", nil)` | no object: `Model: "gemini:gemini-2.5-pro"` |
| `llmagent.New(llmagent.Config{Name, Model, Instruction, Tools})` | `kiln.New(kiln.Config{Model, Instructions, Tools})` |
| `functiontool.New` | `kiln.NewTool(name, description, func(ctx, in))` |
| `runner.New(runner.Config{Agent, SessionService})` | no runner: `agent.Generate(ctx, prompt)` |
| `session.InMemoryService()` | built in (`threads_inmem`); use `agent.Thread(id)` |
| `SubAgents` / transfer | `expert.AsTool(name, description)` |
| callbacks | modules with `WrapModel`, `WrapTool`, `BeforeStep` |
| deploy config | `Config.RunIn` (the whole agent in a sandbox) |

The smallest program is one import:

```go
agent, err := kiln.New(kiln.Config{Model: "mock:demo"})
if err != nil {
	log.Fatal(err)
}
defer agent.Close()

res, err := agent.Generate(context.Background(), "Hello")
fmt.Println(res.Text)
```

Security is on by default: modules reach I/O only through the broker, and
`kiln.New` uses `SecurePolicy` unless you pass `Config.Policy`.
