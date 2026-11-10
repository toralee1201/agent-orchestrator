package qoder

import (
	"context"
	"reflect"
	"testing"

	workerqoder "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/qoder"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestReviewCommandKeepsDefaultPermissionAndGrantedRoot(t *testing.T) {
	if New().Harness() != domain.ReviewerQoder {
		t.Fatalf("harness = %q", New().Harness())
	}
	spec, err := (&Reviewer{plugin: workerqoder.NewWithBinary("qodercli")}).ReviewCommand(context.Background(), ports.ReviewInvocation{
		SystemPrompt: "review only", Config: domain.AgentConfig{Model: "GLM-5.3", Effort: "low"},
		TaskPromptRoot: "/ao/prompts/reviewer", Prompt: "read the task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Argv) < 2 || spec.Argv[1] != "--permission-mode" || spec.Argv[2] != "default" {
		t.Fatalf("argv = %#v", spec.Argv)
	}
	wantTail := []string{"--model", "GLM-5.3", "--reasoning-effort", "low", "--append-system-prompt", "review only", "--add-dir", "/ao/prompts/reviewer"}
	if !reflect.DeepEqual(spec.Argv[len(spec.Argv)-len(wantTail):], wantTail) {
		t.Fatalf("argv = %#v", spec.Argv)
	}
}
