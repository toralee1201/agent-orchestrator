package codebuddy

import (
	"context"
	"reflect"
	"testing"

	workercodebuddy "github.com/aoagents/agent-orchestrator/backend/internal/adapters/agent/codebuddy"
	"github.com/aoagents/agent-orchestrator/backend/internal/domain"
	"github.com/aoagents/agent-orchestrator/backend/internal/ports"
)

func TestReviewCommandKeepsDefaultPermissionAndGrantedRoot(t *testing.T) {
	if New().Harness() != domain.ReviewerCodeBuddy {
		t.Fatalf("harness = %q", New().Harness())
	}
	spec, err := (&Reviewer{plugin: workercodebuddy.NewWithBinary("codebuddy")}).ReviewCommand(context.Background(), ports.ReviewInvocation{
		SystemPrompt: "review only", Config: domain.AgentConfig{Model: "glm-5.3", Effort: "low"},
		TaskPromptRoot: "/ao/prompts/reviewer", Prompt: "read the task",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Argv) < 2 || spec.Argv[1] != "--permission-mode" || spec.Argv[2] != "default" {
		t.Fatalf("argv = %#v", spec.Argv)
	}
	wantTail := []string{"--model", "glm-5.3", "--effort", "low", "--append-system-prompt", "review only", "--add-dir", "/ao/prompts/reviewer"}
	if !reflect.DeepEqual(spec.Argv[len(spec.Argv)-len(wantTail):], wantTail) {
		t.Fatalf("argv = %#v", spec.Argv)
	}
}
