/*
Copyright 2026 The littlered Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package redis

import "testing"

// TestParseMigrationTasks guards the lenient CLUSTER MIGRATION STATUS parser against
// both the RESP2 (array of field/value) and RESP3 (map) task encodings. The exact wire
// shape is confirmed against a live Redis 8.4.x on the lab; this is the repeatable guard.
func TestParseMigrationTasks(t *testing.T) {
	// RESP2: outer array of tasks, each an array of alternating field/value.
	resp2 := []any{
		[]any{"id", "abc123", "state", "in_progress", "last_error", ""},
		[]any{"id", "def456", "state", "completed", "last_error", ""},
	}
	tasks := parseMigrationTasks(resp2)
	if len(tasks) != 2 {
		t.Fatalf("RESP2: expected 2 tasks, got %d", len(tasks))
	}
	if tasks[0].ID != "abc123" || tasks[0].State != "in_progress" {
		t.Errorf("RESP2: task0 = %+v", tasks[0])
	}
	if tasks[1].State != "completed" {
		t.Errorf("RESP2: task1 state = %q, want completed", tasks[1].State)
	}

	// RESP3: outer array of tasks, each a map.
	resp3 := []any{
		map[any]any{"id": "z9", "state": "failed", "last_error": "boom"},
	}
	tasks = parseMigrationTasks(resp3)
	if len(tasks) != 1 || tasks[0].State != "failed" || tasks[0].LastError != "boom" {
		t.Fatalf("RESP3: got %+v", tasks)
	}

	// Non-array / empty replies parse to no tasks.
	if got := parseMigrationTasks("not-an-array"); got != nil {
		t.Errorf("expected nil for non-array reply, got %+v", got)
	}
}

func TestMigrationTerminal(t *testing.T) {
	for _, s := range []string{"completed", "cancelled", "canceled", "failed", "", "COMPLETED"} {
		if !migrationTerminal(s) {
			t.Errorf("expected %q terminal", s)
		}
	}
	for _, s := range []string{"in_progress", "importing", "trimming"} {
		if migrationTerminal(s) {
			t.Errorf("expected %q non-terminal", s)
		}
	}
}
