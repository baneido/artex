package server

import (
	"encoding/json"
	"regexp"
	"testing"
)

var tsukaiKana = regexp.MustCompile(`[\x{3040}-\x{30ff}]`)

// TestRefreshToolDescriptionsJaKeepsBindings checks that a database seeded with the
// former Chinese description gets the Japanese default, that the user's agent binding
// and enabled flag survive, and that the refresh runs only once.
func TestRefreshToolDescriptionsJaKeepsBindings(t *testing.T) {
	s, _, _ := trafficEvidenceServer(t)
	pg := s.m.pg
	const key = "list_findings"
	old, err := pg.GetTool(key)
	if err != nil || old == nil {
		t.Fatal("missing tool", key, err)
	}
	t.Cleanup(func() {
		bindings, _ := json.Marshal(old.Agents)
		pg.UpdateTool(key, old.Description, old.Schema, bindings, old.Enabled)
		_ = pg.SetSetting("tool_descriptions_ja_v1", "true")
	})
	if err := pg.UpdateTool(key, "列出确认漏洞", old.Schema, json.RawMessage(`["custom-agent"]`), false); err != nil {
		t.Fatal(err)
	}
	if err := pg.SetSetting("tool_descriptions_ja_v1", "false"); err != nil {
		t.Fatal(err)
	}
	s.refreshToolDescriptionsJa()
	row, _ := pg.GetTool(key)
	if !tsukaiKana.MatchString(row.Description) || row.Description == "列出确认漏洞" {
		t.Fatalf("description not refreshed: %q", row.Description)
	}
	if row.Enabled || len(row.Agents) != 1 || row.Agents[0] != "custom-agent" {
		t.Fatalf("binding or enabled flag changed: enabled=%v agents=%v", row.Enabled, row.Agents)
	}
	// Second run must be a no-op even if the user edits the description again.
	if err := pg.UpdateTool(key, "USER DESCRIPTION", row.Schema, json.RawMessage(`["custom-agent"]`), false); err != nil {
		t.Fatal(err)
	}
	s.refreshToolDescriptionsJa()
	if again, _ := pg.GetTool(key); again.Description != "USER DESCRIPTION" {
		t.Fatalf("refresh ran twice: %q", again.Description)
	}
}
