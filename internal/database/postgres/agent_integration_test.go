package postgres

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/swqa7697/data-mate/internal/agent"
	"github.com/swqa7697/data-mate/internal/config"
	"github.com/swqa7697/data-mate/internal/vault"
)

// Ladder 2: extend the owned PG scenario rather than reproduce its SQL policy
// corpus. Explicit opt-in uses normal authenticated clients and temporary exact
// registration identities, removes only those entries, and retains retry state
// on cleanup failure. No authentication settings or approval policy are changed.
func nativeAgentAcceptance(t *testing.T, p config.Profile, password string, sql func(string, ...any)) {
	t.Helper()
	if os.Getenv("DATA_MATE_AGENT_TEST") != "1" {
		return
	}
	sql("CREATE TABLE app.native_agent_items(id int); INSERT INTO app.native_agent_items VALUES(1),(2); GRANT SELECT ON app.native_agent_items TO reader")
	defer sql("DROP TABLE app.native_agent_items")
	ctx, cancel := context.WithTimeout(t.Context(), 12*time.Minute)
	defer cancel()
	dir, err := os.MkdirTemp("/tmp", "data-mate-native-agents-")
	if err != nil {
		t.Fatal(err)
	}
	rootPath := filepath.Join(dir, ".dev")
	if err = os.MkdirAll(filepath.Join(rootPath, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	root, err := config.ResolveRoot(rootPath, "")
	if err != nil {
		t.Fatal(err)
	}
	binary := filepath.Join(root.Path, "bin/data-mate")
	cmd := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-trimpath", "-o", binary, "./cmd/data-mate")
	cmd.Dir = "../../.."
	if out, e := cmd.CombinedOutput(); e != nil {
		t.Fatalf("native agent build: %v %s", e, out)
	}
	if err = os.Chmod(binary, 0700); err != nil {
		t.Fatal(err)
	}
	run := func(input string, args ...string) ([]byte, error) {
		c := exec.CommandContext(ctx, binary, args...)
		c.Dir = dir
		c.Stdin = strings.NewReader(input)
		c.WaitDelay = time.Second
		return c.CombinedOutput()
	}
	store, err := config.Open(ctx, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	clean := true
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		stop := exec.CommandContext(cleanup, binary, "mcp", "stop", "--json")
		stop.Dir = dir
		if _, e := stop.Output(); e != nil {
			clean = false
			t.Error("native agent service cleanup", e)
		}
		l, e := store.Lifecycle(cleanup)
		if e == nil {
			e = agent.New(root).RemoveOwned(cleanup, l)
			l.Release()
		}
		if e != nil {
			clean = false
			t.Error("native agent registration cleanup", e)
		}
		if e = (vault.Keychain{}).Delete(cleanup, root.Digest); e != nil {
			clean = false
			t.Error("native agent key cleanup", e)
		}
		store.Close()
		if clean {
			if e = os.RemoveAll(dir); e != nil {
				t.Error(e)
			}
		} else {
			t.Log("native retry root retained", root.Path)
		}
	}()
	args := []string{"db", "add", "--alias", "fixture", "--host", p.Connection.Host, "--port", fmt.Sprint(p.Connection.Port), "--database", p.Connection.Database, "--username", p.Connection.Username, "--password-stdin", "--schema", "app", "--yes"}
	if out, e := run(password+"\n", args...); e != nil {
		t.Fatalf("native profile: %v %s", e, out)
	}
	if out, e := run("", "mcp", "start", "--json"); e != nil {
		t.Fatalf("native start: %v %s", e, out)
	}
	name := agent.Name(root)
	for _, client := range []string{"codex", "claude"} {
		version, e := exec.CommandContext(ctx, client, "--version").Output()
		if e != nil {
			t.Fatal(e)
		}
		t.Logf("native client %s", strings.TrimSpace(string(version)))
		prompt := fmt.Sprintf("Use only the MCP server %s. Call list_connections, list_tables with connection fixture and schema app, describe_table for fixture/app/native_agent_items, and query with connection fixture, sql SELECT id FROM app.native_agent_items ORDER BY id and row_limit 1. Then call query with sql SELECT app.policy_probe() and verify it returns QUERY_UNSUPPORTED. Do not use shell, files, network, or other servers. Report the actual results, including errors. Do not claim success unless these five calls ran.", name)
		raw := runNativeAgent(t, ctx, dir, client, name, prompt, false)
		for _, tool := range []string{"list_connections", "list_tables", "describe_table", "query"} {
			if !hasNativeTool(raw, client, name, tool) {
				t.Fatalf("%s did not execute %s; %s", client, tool, nativeSummary(raw))
			}
		}
		if !nativeBoundedQuery(nativeResults(raw, client, name, "query")) {
			t.Fatalf("%s did not return one truncated row", client)
		}
		if !bytes.Contains(nativeResults(raw, client, name, "query"), []byte("QUERY_UNSUPPORTED")) {
			t.Fatalf("%s omitted shared policy rejection: %s", client, nativeSummary(raw))
		}
		t.Logf("%s all four tools and shared policy rejection passed", client)
	}
	if out, e := run("", "db", "scope", "fixture", "--none", "--yes"); e != nil {
		t.Fatalf("native narrow scope: %v %s", e, out)
	}
	for _, client := range []string{"codex", "claude"} {
		prompt := fmt.Sprintf("Use only MCP server %s. Call query with connection fixture and sql SELECT id FROM app.native_agent_items. Report the error code. Do not use any other tools or modify settings.", name)
		raw := runNativeAgent(t, ctx, dir, client, name, prompt, false)
		if !hasNativeTool(raw, client, name, "query") || !bytes.Contains(nativeResults(raw, client, name, "query"), []byte("SCOPE_DENIED")) {
			t.Fatalf("%s scope denial missing: %s", client, nativeSummary(raw))
		}
		t.Logf("%s scope narrowing denied query", client)
	}
	// A native explicit deny overrides the otherwise allowed read-only tool.
	prompt := fmt.Sprintf("Try calling the query tool on server %s with connection fixture and sql SELECT 1. Report if permission prevents the call. Do not use other tools or modify settings.", name)
	raw := runNativeAgent(t, ctx, dir, "claude", name, prompt, true)
	if results := nativeResults(raw, "claude", name, "query"); len(results) > 0 && !bytes.Contains(bytes.ToLower(results), []byte("permission")) && !bytes.Contains(bytes.ToLower(results), []byte("denied")) {
		t.Fatal("native denied tool executed")
	}
	if !nativeToolDenied(raw, name) {
		t.Fatal("Claude deny did not remove the query capability")
	}
	t.Log("Claude explicit query deny preserved")
	if out, e := run("", "mcp", "stop", "--json"); e != nil {
		t.Fatalf("native stop: %v %s", e, out)
	}
	for _, client := range []string{"codex", "claude"} {
		prompt := fmt.Sprintf("Try listing connections from MCP server %s. If unavailable, report unavailable. Do not start any process or use other tools or modify settings.", name)
		raw := runNativeAgent(t, ctx, dir, client, name, prompt, false)
		if hasNativeTool(raw, client, name, "list_connections") {
			t.Fatalf("%s executed stopped service call", client)
		}
		t.Logf("%s stopped service unavailable", client)
	}
	if out, e := run("", "mcp", "status", "--json"); e != nil || !bytes.Contains(out, []byte(`"state":"stopped"`)) {
		t.Fatalf("native sessions restarted service: %v", e)
	}
}

func runNativeAgent(t *testing.T, parent context.Context, dir, client, name, prompt string, deny bool) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	args := []string{"exec", "--skip-git-repo-check", "--ephemeral", "--json", prompt}
	if client == "claude" {
		args = []string{"-p", "--verbose", "--output-format", "stream-json", "--no-session-persistence", "--permission-mode", "manual", "--tools", "", "--allowedTools", "mcp__" + name + "__*"}
		if deny {
			args = append(args, "--disallowedTools", "mcp__"+name+"__query")
		}
		args = append(args, "--", prompt)
	}
	cmd := exec.CommandContext(ctx, client, args...)
	cmd.Dir = dir
	cmd.WaitDelay = time.Second
	cmd.SysProcAttr = &unix.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return unix.Kill(-cmd.Process.Pid, unix.SIGKILL) }
	var out nativeOutput
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("native %s session failed (%v): %s", client, err, nativeSummary(out.Bytes()))
	}
	for _, tool := range []string{"list_connections", "list_tables", "describe_table", "query"} {
		if result := nativeResults(out.Bytes(), client, name, tool); len(result) > 0 {
			t.Logf("native %s %s result: %s", client, tool, result)
		}
	}
	return out.Bytes()
}

type nativeOutput struct{ bytes.Buffer }

func (b *nativeOutput) Write(p []byte) (int, error) {
	if b.Len()+len(p) > 8<<20 {
		return 0, fmt.Errorf("native client output bound")
	}
	return b.Buffer.Write(p)
}

// Correlate Claude tool results to invocation IDs; Codex completes both success
// and MCP error calls. Agent prose alone never proves execution or rejection.
func nativeResults(raw []byte, client, name, tool string) []byte {
	ids := map[string]bool{}
	var results [][]byte
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var event map[string]any
		if json.Unmarshal(line, &event) != nil {
			continue
		}
		if client == "codex" {
			item, _ := event["item"].(map[string]any)
			if event["type"] == "item.completed" && item["type"] == "mcp_tool_call" && item["server"] == name && item["tool"] == tool {
				result, _ := item["result"].(map[string]any)
				b, _ := json.Marshal(result["structured_content"])
				if result == nil {
					b, _ = json.Marshal(item["error"])
				}
				results = append(results, b)
			}
		} else {
			message, _ := event["message"].(map[string]any)
			content, _ := message["content"].([]any)
			for _, v := range content {
				block, _ := v.(map[string]any)
				if block["type"] == "tool_use" && block["name"] == "mcp__"+name+"__"+tool {
					id, _ := block["id"].(string)
					ids[id] = true
				}
				if block["type"] == "tool_result" {
					id, _ := block["tool_use_id"].(string)
					if ids[id] {
						b, _ := json.Marshal(block)
						if text, ok := block["content"].(string); ok && json.Valid([]byte(text)) {
							b = []byte(text)
						}
						results = append(results, b)
					}
				}
			}
		}
	}
	return bytes.Join(results, []byte("\n"))
}
func nativeBoundedQuery(raw []byte) bool {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var result struct {
			Rows      [][]any `json:"rows"`
			Count     int     `json:"row_count"`
			Truncated bool    `json:"truncated"`
		}
		if json.Unmarshal(line, &result) == nil && len(result.Rows) == 1 && len(result.Rows[0]) == 1 && result.Rows[0][0] == float64(1) && result.Count == 1 && result.Truncated {
			return true
		}
	}
	return false
}
func hasNativeTool(raw []byte, client, name, tool string) bool {
	return len(nativeResults(raw, client, name, tool)) > 0
}
func nativeToolDenied(raw []byte, name string) bool {
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var e struct {
			Type    string   `json:"type"`
			Subtype string   `json:"subtype"`
			Tools   []string `json:"tools"`
		}
		if json.Unmarshal(line, &e) != nil || e.Type != "system" || e.Subtype != "init" {
			continue
		}
		other := false
		for _, tool := range e.Tools {
			if tool == "mcp__"+name+"__query" {
				return false
			}
			if tool == "mcp__"+name+"__list_connections" {
				other = true
			}
		}
		return other
	}
	return false
}
func nativeSummary(raw []byte) string {
	// Keep only the final text/result, never unrelated configuration or init events.
	var result string
	for _, line := range bytes.Split(raw, []byte("\n")) {
		var e map[string]any
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		if e["type"] == "result" {
			result, _ = e["result"].(string)
		}
		if item, ok := e["item"].(map[string]any); ok && item["type"] == "agent_message" {
			result, _ = item["text"].(string)
		}
	}
	if len(result) > 1500 {
		result = result[:1500]
	}
	return result
}
