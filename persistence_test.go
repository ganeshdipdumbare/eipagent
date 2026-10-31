package eipagent_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/ganeshdipdumbare/eipagent"
	_ "github.com/ganeshdipdumbare/eipagent/drivers/postgres"
)

// persistenceSuite checks durable threads with any checkpointer/store setup.
func persistenceSuite(t *testing.T, opts ...eipagent.Option) {
	var deleted []string
	script := []any{
		map[string]any{"tool": "delete_file", "args": map[string]any{"path": "x"}},
		map[string]any{"text": "deleted"},
	}
	hitl := eipagent.WithPlugin("mw_hitl", map[string]any{"interrupt_on": map[string]any{"delete_file": true}})
	run := fmt.Sprintf("%s-%d", t.Name(), time.Now().UnixNano()) // shared DBs outlive test runs
	thread := "user-1/" + run

	// Replica A pauses on an approval...
	a := mockAgent(t, script, append(opts, eipagent.WithTools(deleteTool(&deleted)), hitl)...)
	res, err := a.Thread(thread).Generate(bg, "delete x")
	if err != nil || !res.Interrupted() {
		t.Fatalf("A: %v %+v", err, res)
	}
	a.Close()

	// ...replica B (a fresh process) resumes it.
	b := mockAgent(t, script[1:], append(opts, eipagent.WithTools(deleteTool(&deleted)), hitl)...)
	pending, err := b.Thread(thread).Pending(bg)
	if err != nil || len(pending) != 1 {
		t.Fatalf("B should see the pending interrupt: %v %v", err, pending)
	}
	res, err = b.Thread(thread).Resume(bg, eipagent.Approve(pending[0].ID))
	if err != nil || res.Text != "deleted" || len(deleted) != 1 {
		t.Fatalf("B resume: %v %+v", err, res)
	}

	// Concurrent writers: a tool that writes the same thread mid-run.
	var inner error
	var agent *eipagent.Agent
	sneaky := eipagent.Tool("sneaky", "s", func(ctx context.Context, in struct{}) (string, error) {
		_, inner = agent.Thread(thread).Generate(context.Background(), "interleaved")
		return "ok", nil
	})
	agent = mockAgent(t, []any{map[string]any{"tool": "sneaky"}, map[string]any{"text": "inner"}, map[string]any{"text": "outer"}}, append(opts, eipagent.WithTools(sneaky))...)
	_, err = agent.Thread(thread).Generate(bg, "go")
	if inner != nil || !errors.Is(err, eipagent.ErrThreadBusy) {
		t.Fatalf("outer save must conflict: inner=%v outer=%v", inner, err)
	}

	// Long-term memory survives restarts too.
	m1 := mockAgent(t, []any{map[string]any{"tool": "remember", "args": map[string]any{"text": "likes tea"}}, map[string]any{"text": "saved"}},
		append(opts, eipagent.WithPlugins("memory_store"), eipagent.WithUser("u-"+run))...)
	if _, err := m1.Generate(bg, "remember tea"); err != nil {
		t.Fatal(err)
	}
	m1.Close()
	m2 := mockAgent(t, []any{map[string]any{"tool": "recall", "args": map[string]any{"query": "tea"}}}, append(opts, eipagent.WithPlugins("memory_store"), eipagent.WithUser("u-"+run))...)
	res, err = m2.Generate(bg, "what do I like")
	if err != nil || res.Text != "likes tea" {
		t.Fatalf("recall after restart: %v %q", err, res.Text)
	}
}

func TestFilePersistence(t *testing.T) {
	dir := t.TempDir()
	persistenceSuite(t,
		eipagent.WithPlugin("checkpoint_file", map[string]any{"dir": dir + "/threads"}),
		eipagent.WithPlugin("store_file", map[string]any{"path": dir + "/store.json"}))
}

// Run with EIP_TEST_PG_DSN=postgres://... go test -run Postgres
func TestPostgresPersistence(t *testing.T) {
	dsn := os.Getenv("EIP_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set EIP_TEST_PG_DSN to run")
	}
	t.Setenv("EIP_DATABASE_URL", dsn)
	persistenceSuite(t,
		eipagent.WithPluginSpec(eipagent.PluginSpec{Use: "secrets_env", Config: map[string]any{"allow": []any{"EIP_DATABASE_URL", "*_API_KEY"}}}),
		eipagent.WithPlugins("db_sql", "checkpoint_sql", "store_sql"))
}
