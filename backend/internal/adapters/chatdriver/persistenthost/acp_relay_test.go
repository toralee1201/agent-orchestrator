package persistenthost

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestACPRelayCapturesIndependentForkForReconnect(t *testing.T) {
	for _, childID := range []string{"child", "", "parent"} {
		t.Run(childID, func(t *testing.T) {
			relay := newTestACPRelay(t)
			provider := relayClientFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/fork","params":{"sessionId":"parent","cwd":"/tmp"}}`+"\n"), 1)
			result, _ := json.Marshal(map[string]any{"sessionId": childID, "configOptions": []any{}})
			relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":`+frameID(t, provider)+`,"result":`+string(result)+`}`+"\n"), 1, true)
			state := relay.snapshot()
			if childID == "child" {
				if state.SessionID != "child" || len(state.SessionResult) == 0 {
					t.Fatalf("lost child reconnect state: %+v", state)
				}
			} else if state.SessionID != "" || len(state.SessionResult) != 0 {
				t.Fatalf("malformed fork adopted parent ownership: %+v", state)
			}
		})
	}
}

func TestACPRelayRetainsAvailableCommandsForReconnect(t *testing.T) {
	relay := newTestACPRelay(t)
	provider := relayClientFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/new","params":{"cwd":"/tmp","mcpServers":[]}}`+"\n"), 1)
	relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":`+frameID(t, provider)+`,"result":{"sessionId":"live"}}`+"\n"), 1, true)

	update := func(session, name string) {
		frame := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"` + session +
			`","update":{"sessionUpdate":"available_commands_update","availableCommands":[{"name":"` + name + `","description":"d"}]}}}` + "\n"
		if forwarded, retained := relayProviderFrame(t, relay, []byte(frame), 1, true); retained || len(forwarded) == 0 {
			t.Fatalf("catalog update was not forwarded: retained=%v frame=%q", retained, forwarded)
		}
	}
	update("live", "review")
	update("live", "compact")
	if got := string(relay.snapshot().AvailableCommands); got != `[{"name":"compact","description":"d"}]` {
		t.Fatalf("retained catalog = %s", got)
	}

	// A catalog for a session the host no longer owns must not be handed to the
	// controller that reattaches to the live one.
	update("other", "stale")
	if got := relay.snapshot().AvailableCommands; len(got) != 0 {
		t.Fatalf("foreign catalog leaked into snapshot: %s", got)
	}
}

func TestACPRelayReclaimsPromptAcrossAttachment(t *testing.T) {
	relay := newTestACPRelay(t)

	providerInit := relayClientFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"), 1)
	initID := frameID(t, providerInit)
	clientInit, journaled := relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":`+initID+`,"result":{"protocolVersion":1,"agentCapabilities":{},"agentInfo":{"name":"fake","version":"1"}}}`+"\n"), 1, true)
	if journaled || frameID(t, clientInit) != "1" {
		t.Fatalf("initialize relay = journaled:%v frame:%s", journaled, clientInit)
	}

	providerSession := relayClientFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp","mcpServers":[]}}`+"\n"), 1)
	sessionID := frameID(t, providerSession)
	relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":`+sessionID+`,"result":{"sessionId":"session-live"}}`+"\n"), 1, true)

	providerPrompt := relayClientFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"session-live","prompt":[]}}`+"\n"), 1)
	promptID := frameID(t, providerPrompt)

	update, journaled := relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-live","update":{"agent_message_chunk":{"content":{"type":"text","text":"survived"}}}}}`+"\n"), 1, true)
	if !journaled || !strings.Contains(string(update), ACPEventIDMetaKey) {
		t.Fatalf("prompt update = journaled:%v frame:%s", journaled, update)
	}

	response, journaled := relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":`+promptID+`,"result":{"stopReason":"end_turn"}}`+"\n"), 1, true)
	if !journaled || frameID(t, response) != "3" || !strings.Contains(string(response), ACPEventIDMetaKey) {
		t.Fatalf("prompt response = journaled:%v frame:%s", journaled, response)
	}

	state := relay.snapshot()
	if state.SessionID != "session-live" || state.ActivePrompt ||
		state.PendingResultEventID == "" || len(state.InitializeResult) == 0 {
		t.Fatalf("reconnect state = %+v", state)
	}
	replay := relayReplayFrames(t, relay)
	if len(replay) != 2 || !strings.Contains(string(replay[0]), "session/update") ||
		!strings.Contains(string(replay[1]), ACPPromptResultMethod) {
		t.Fatalf("replay frames = %q", replay)
	}

	// A new SDK starts its local request IDs over. The host still allocates a
	// fresh provider-side ID, so it cannot collide with the old prompt.
	providerNext := relayClientFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/set_mode","params":{}}`+"\n"), 2)
	if nextID := frameID(t, providerNext); nextID == "1" || nextID == promptID {
		t.Fatalf("replacement request reused provider id %s", nextID)
	}

	if frame := relayClientFrame(t, relay, []byte(`{"jsonrpc":"2.0","method":"_ao/persistent_prompt_ack","params":{"eventId":"wrong"}}`+"\n"), 2); len(frame) != 0 {
		t.Fatalf("private ack leaked to provider: %s", frame)
	}
	if replay := relayReplayFrames(t, relay); len(replay) != 2 {
		t.Fatalf("mismatched ack discarded %d replay frames", len(replay))
	}
	ack, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": ACPPromptAckMethod,
		"params": map[string]string{"eventId": state.PendingResultEventID},
	})
	if frame := relayClientFrame(t, relay, append(ack, '\n'), 2); len(frame) != 0 {
		t.Fatalf("private ack leaked to provider: %s", frame)
	}
	if replay := relayReplayFrames(t, relay); len(replay) != 0 {
		t.Fatalf("acknowledged replay retained %d frames", len(replay))
	}
}

func TestACPRelayTracksActiveCompactionPrompt(t *testing.T) {
	relay := newTestACPRelay(t)

	// Start /compact prompt
	providerPrompt := relayClientFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{"sessionId":"session-live","prompt":[{"type":"text","text":"/compact"}]}}`+"\n"), 1)
	promptID := frameID(t, providerPrompt)

	state := relay.snapshot()
	if !state.ActivePrompt {
		t.Fatal("ActivePrompt = false, want true")
	}
	if !state.ActiveCompaction {
		t.Fatal("ActiveCompaction = false, want true for /compact prompt")
	}

	// Complete prompt
	relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":`+promptID+`,"result":{"stopReason":"end_turn"}}`+"\n"), 1, true)

	stateAfter := relay.snapshot()
	if stateAfter.ActivePrompt {
		t.Fatal("ActivePrompt = true, want false after completion")
	}
	if stateAfter.ActiveCompaction {
		t.Fatal("ActiveCompaction = true, want false after completion")
	}
}

func TestACPRelayGivesReplayedProviderRequestStableIdentity(t *testing.T) {
	relay := newTestACPRelay(t)
	frame := []byte(`{"jsonrpc":"2.0","id":"permission-7","method":"session/request_permission","params":{"sessionId":"s","options":[]}}` + "\n")
	first, _ := relayProviderFrame(t, relay, frame, 1, true)
	second, _ := relayProviderFrame(t, relay, frame, 2, true)
	firstID := frameMetaString(t, first, ACPRequestIDMetaKey)
	secondID := frameMetaString(t, second, ACPRequestIDMetaKey)
	if firstID == "" || firstID != secondID {
		t.Fatalf("stable request identities = %q, %q", firstID, secondID)
	}
	relayClientFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":"permission-7","result":{"outcome":"cancelled"}}`+"\n"), 2)
	third, _ := relayProviderFrame(t, relay, frame, 2, true)
	thirdID := frameMetaString(t, third, ACPRequestIDMetaKey)
	if thirdID == firstID {
		t.Fatalf("reused provider wire id retained completed interaction identity %q", thirdID)
	}
}

func TestACPRelayAcceptsInteractionCommandIdempotently(t *testing.T) {
	relay := newTestACPRelay(t)
	prompt := relayClientFrame(t, relay,
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{}}`+"\n"), 1)
	request, _ := relayProviderFrame(t, relay,
		[]byte(`{"jsonrpc":"2.0","id":"permission-7","method":"session/request_permission","params":{}}`+"\n"),
		1, true)
	requestID := frameMetaString(t, request, ACPRequestIDMetaKey)
	command := func(id int, decision string) acpClientFrames {
		t.Helper()
		frame, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": id, "method": ACPInteractionCommandMethod,
			"params": map[string]any{
				"requestId": requestID, "kind": "approval",
				"decision": map[string]string{"id": decision},
			},
		})
		result, err := relay.clientFrame(context.Background(), append(frame, '\n'), 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(result.provider) != 0 {
			t.Fatalf("private command leaked to provider: %s", result.provider)
		}
		return result
	}
	eventID := frameResultString(t, command(10, "allow").client, "eventId")
	if eventID == "" || frameResultString(t, command(11, "allow").client, "eventId") != eventID {
		t.Fatal("identical command was not acknowledged with one stable event id")
	}
	if changed := command(12, "reject").client; !bytes.Contains(changed, []byte(`"error"`)) {
		t.Fatalf("changed command was accepted: %s", changed)
	}

	replay := relayReplayFrames(t, relay)
	if len(replay) != 2 || !bytes.Contains(replay[1], []byte(`"providerPending":true`)) {
		t.Fatalf("pending interaction replay = %q", replay)
	}
	relayClientFrame(t, relay,
		[]byte(`{"jsonrpc":"2.0","id":"permission-7","result":{"outcome":"cancelled"}}`+"\n"), 1)
	replay = relayReplayFrames(t, relay)
	if len(replay) != 1 || !bytes.Contains(replay[0], []byte(`"providerPending":false`)) {
		t.Fatalf("settled interaction replay = %q", replay)
	}
	relayProviderFrame(t, relay,
		[]byte(`{"jsonrpc":"2.0","id":`+frameID(t, prompt)+`,"result":{"stopReason":"end_turn"}}`+"\n"),
		1, true)
}

func TestACPRelayPreservesInteractionCausalityAcrossReplay(t *testing.T) {
	relay := newTestACPRelay(t)
	prompt := relayClientFrame(t, relay,
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{}}`+"\n"), 1)
	relayProviderFrame(t, relay,
		[]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"marker":"before"}}`+"\n"),
		1, true)
	request, _ := relayProviderFrame(t, relay,
		[]byte(`{"jsonrpc":"2.0","id":"permission-7","method":"session/request_permission","params":{}}`+"\n"),
		1, true)
	requestID := frameMetaString(t, request, ACPRequestIDMetaKey)
	command := []byte(`{"jsonrpc":"2.0","id":10,"method":"` + ACPInteractionCommandMethod +
		`","params":{"requestId":"` + requestID + `","kind":"approval","decision":{"id":"allow"}}}` + "\n")
	if _, err := relay.clientFrame(context.Background(), command, 1); err != nil {
		t.Fatal(err)
	}
	relayClientFrame(t, relay,
		[]byte(`{"jsonrpc":"2.0","id":"permission-7","result":{"outcome":"selected"}}`+"\n"), 1)
	relayProviderFrame(t, relay,
		[]byte(`{"jsonrpc":"2.0","method":"session/update","params":{"marker":"after"}}`+"\n"),
		1, true)
	relayProviderFrame(t, relay,
		[]byte(`{"jsonrpc":"2.0","id":`+frameID(t, prompt)+`,"result":{"stopReason":"end_turn"}}`+"\n"),
		1, true)

	replay := relayReplayFrames(t, relay)
	if len(replay) != 4 || !bytes.Contains(replay[0], []byte(`"marker":"before"`)) ||
		!bytes.Contains(replay[1], []byte(ACPInteractionCommandMethod)) ||
		!bytes.Contains(replay[2], []byte(`"marker":"after"`)) ||
		!bytes.Contains(replay[3], []byte(ACPPromptResultMethod)) {
		t.Fatalf("causal replay order = %q", replay)
	}
}

func TestACPRelayReplaysProviderCancellationAfterItsRequest(t *testing.T) {
	relay := newTestACPRelay(t)
	relayClientFrame(t, relay,
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{}}`+"\n"), 1)
	relayProviderFrame(t, relay,
		[]byte(`{"jsonrpc":"2.0","id":"permission-7","method":"session/request_permission","params":{}}`+"\n"),
		1, true)
	relayProviderFrame(t, relay,
		[]byte(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":"permission-7"}}`+"\n"),
		1, true)

	replay := relayReplayFrames(t, relay)
	if len(replay) != 2 || !bytes.Contains(replay[0], []byte("session/request_permission")) ||
		!bytes.Contains(replay[1], []byte("$/cancel_request")) {
		t.Fatalf("request/cancellation replay order = %q", replay)
	}
}

func TestACPRelayRewritesClientCancellationToProviderRequestID(t *testing.T) {
	relay := newTestACPRelay(t)
	relayClientFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":99,"method":"initialize","params":{}}`+"\n"), 7)
	request := relayClientFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{}}`+"\n"), 7)
	providerID := frameID(t, request)
	cancel := relayClientFrame(t, relay, []byte(`{"jsonrpc":"2.0","method":"$/cancel_request","params":{"requestId":1}}`+"\n"), 7)
	var envelope struct {
		Params struct {
			RequestID json.RawMessage `json:"requestId"`
		} `json:"params"`
	}
	if err := json.Unmarshal(cancel, &envelope); err != nil {
		t.Fatal(err)
	}
	if string(envelope.Params.RequestID) != providerID {
		t.Fatalf("cancel target = %s, want provider id %s", envelope.Params.RequestID, providerID)
	}
}

func TestACPRelayDeduplicatesSessionCancelAcrossAttachments(t *testing.T) {
	relay := newTestACPRelay(t)
	prompt := relayClientFrame(t, relay,
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"session/prompt","params":{}}`+"\n"), 1)
	cancel := []byte(`{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"s"}}` + "\n")
	if frame := relayClientFrame(t, relay, cancel, 1); len(frame) == 0 {
		t.Fatal("first session/cancel was suppressed")
	}
	if frame := relayClientFrame(t, relay, cancel, 2); len(frame) != 0 {
		t.Fatalf("duplicate session/cancel crossed attachment boundary: %s", frame)
	}

	relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":`+frameID(t, prompt)+`,"result":{"stopReason":"cancelled"}}`+"\n"), 2, true)
	ack, _ := json.Marshal(map[string]any{"method": ACPPromptAckMethod, "params": map[string]string{"eventId": relay.snapshot().PendingResultEventID}})
	relayClientFrame(t, relay, ack, 2)
	relayClientFrame(t, relay,
		[]byte(`{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{}}`+"\n"), 2)
	if frame := relayClientFrame(t, relay, cancel, 2); len(frame) == 0 {
		t.Fatal("new prompt inherited prior cancellation")
	}
}

func newTestACPRelay(t *testing.T) *acpRelay {
	t.Helper()
	relay, err := newACPRelay(context.Background(), filepath.Join(t.TempDir(), "prompt.journal"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = relay.close(context.Background()) })
	return relay
}

func relayReplayFrames(t *testing.T, relay *acpRelay) [][]byte {
	t.Helper()
	var replay bytes.Buffer
	if err := relay.replayTo(context.Background(), &replay); err != nil {
		t.Fatal(err)
	}
	lines := bytes.SplitAfter(replay.Bytes(), []byte{'\n'})
	frames := make([][]byte, 0, len(lines))
	for _, line := range lines {
		if len(line) > 0 {
			frames = append(frames, append([]byte(nil), line...))
		}
	}
	return frames
}

func relayClientFrame(t *testing.T, relay *acpRelay, frame []byte, generation uint64) []byte {
	t.Helper()
	rewritten, err := relay.clientFrame(context.Background(), frame, generation)
	if err != nil {
		t.Fatal(err)
	}
	return rewritten.provider
}

func relayProviderFrame(
	t *testing.T,
	relay *acpRelay,
	frame []byte,
	generation uint64,
	attached bool,
) ([]byte, bool) {
	t.Helper()
	rewritten, retainedByACP, err := relay.providerFrame(context.Background(), frame, generation, attached)
	if err != nil {
		t.Fatal(err)
	}
	return rewritten, retainedByACP
}

func frameID(t *testing.T, frame []byte) string {
	t.Helper()
	var envelope struct {
		ID json.RawMessage `json:"id"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		t.Fatalf("decode frame %q: %v", frame, err)
	}
	return string(envelope.ID)
}

func frameMetaString(t *testing.T, frame []byte, key string) string {
	t.Helper()
	var envelope struct {
		Params struct {
			Meta map[string]any `json:"_meta"`
		} `json:"params"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		t.Fatalf("decode frame %q: %v", frame, err)
	}
	value, _ := envelope.Params.Meta[key].(string)
	return value
}

func frameResultString(t *testing.T, frame []byte, key string) string {
	t.Helper()
	var envelope struct {
		Result map[string]string `json:"result"`
	}
	if err := json.Unmarshal(frame, &envelope); err != nil {
		t.Fatalf("decode frame %q: %v", frame, err)
	}
	return envelope.Result[key]
}

func TestACPRelaySettlesStalledPrompt(t *testing.T) {
	relay, promptID := startStallTestPrompt(t)

	// A prompt whose adapter is still producing output never trips the watchdog.
	update, _ := relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-live","update":{"agent_message_chunk":{"content":{"type":"text","text":"working"}}}}}`+"\n"), 1, true)
	if !strings.Contains(string(update), ACPEventIDMetaKey) {
		t.Fatalf("prompt update not journaled: %s", update)
	}
	if frame := failStalled(t, relay, 1, time.Now()).client; frame != nil {
		t.Fatalf("fresh prompt tripped the stall watchdog: %s", frame)
	}

	stalledAt := time.Now().Add(2 * promptStallTimeout)
	first := failStalled(t, relay, 1, stalledAt)
	// The adapter still owns the turn; the watchdog cancels it and keeps the
	// prompt active until the adapter answers or the grace runs out.
	if want := `{"jsonrpc":"2.0","method":"session/cancel","params":{"sessionId":"session-live"}}` + "\n"; string(first.provider) != want {
		t.Fatalf("adapter frame = %s, want %s", first.provider, want)
	}
	if first.client != nil || !relay.snapshot().ActivePrompt {
		t.Fatalf("watchdog settled the prompt before the adapter answered the cancel: %+v", first)
	}
	if frames := failStalled(t, relay, 1, stalledAt.Add(promptStallCancelGrace/2)); frames.client != nil || frames.provider != nil {
		t.Fatalf("watchdog acted again inside the cancel grace: %+v", frames)
	}

	// A wedged adapter never answers: the relay settles the prompt without it.
	response := failStalled(t, relay, 1, stalledAt.Add(promptStallCancelGrace)).client
	if response == nil || frameID(t, response) != promptID {
		t.Fatalf("stalled prompt response = %s", response)
	}
	var failure struct {
		Error struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(response, &failure); err != nil {
		t.Fatalf("decode stalled response %q: %v", response, err)
	}
	if failure.Error.Code != -32603 || !strings.Contains(failure.Error.Message, "provider stopped responding") {
		t.Fatalf("stalled response error = %+v", failure.Error)
	}
	// An adapter that ignored the cancel may still emit, so the failure must not
	// carry the marker that releases the controller's queue.
	if strings.Contains(string(response), ACPPromptStalledDataKey) {
		t.Fatalf("wedged-adapter failure carries the %s marker: %s", ACPPromptStalledDataKey, response)
	}

	state := relay.snapshot()
	if state.ActivePrompt || state.PendingResultEventID == "" {
		t.Fatalf("post-stall state = %+v", state)
	}
	replay := relayReplayFrames(t, relay)
	if len(replay) == 0 || !strings.Contains(string(replay[len(replay)-1]), ACPPromptResultMethod) {
		t.Fatalf("stall completion missing from replay: %d frames", len(replay))
	}

	// The adapter's own answer to the failed prompt arrives late. It must not
	// reach the controller under the relay's provider id.
	if late, _ := relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":`+promptID+`,"result":{"stopReason":"cancelled"}}`+"\n"), 1, true); len(late) != 0 {
		t.Fatalf("late adapter response leaked: %s", late)
	}

	// The turn is terminal: the watchdog must not fire again, and once the
	// controller acknowledges the synthetic completion, the session accepts a
	// new prompt instead of refusing it as still-active.
	if frame := failStalled(t, relay, 1, time.Now().Add(2*promptStallTimeout)).client; frame != nil {
		t.Fatalf("settled stall fired twice: %s", frame)
	}
	ack, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": ACPPromptAckMethod,
		"params": map[string]string{"eventId": state.PendingResultEventID},
	})
	if frame := relayClientFrame(t, relay, append(ack, '\n'), 1); len(frame) != 0 {
		t.Fatalf("private ack leaked to provider: %s", frame)
	}
	if provider := relayClientFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":4,"method":"session/prompt","params":{"sessionId":"session-live","prompt":[]}}`+"\n"), 1); len(provider) == 0 {
		t.Fatalf("session refused a new prompt after the stall settlement")
	}
}

// A restarted daemon adopts the running turn and waits for its completion, so
// the watchdog must still cover a prompt an earlier controller sent.
func TestACPRelaySettlesStalledPromptAfterReattach(t *testing.T) {
	relay, _ := startStallTestPrompt(t)

	stalledAt := time.Now().Add(2 * promptStallTimeout)
	failStalled(t, relay, 2, stalledAt)
	frame := failStalled(t, relay, 2, stalledAt.Add(promptStallCancelGrace)).client
	if !strings.Contains(string(frame), ACPPromptResultMethod) || !strings.Contains(string(frame), "provider stopped responding") {
		t.Fatalf("reattached controller got %s, want the private prompt result carrying the failure", frame)
	}
}

// A turn parked on a permission or elicitation is silent by design; the
// daemon waits up to approvalWait for the user.
func TestACPRelayStallWatchdogWaitsOnTheClient(t *testing.T) {
	relay, _ := startStallTestPrompt(t)
	permission, _ := relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":0,"method":"session/request_permission","params":{"sessionId":"session-live","toolCall":{"toolCallId":"t1"},"options":[]}}`+"\n"), 1, true)

	if frame := failStalled(t, relay, 1, time.Now().Add(2*promptStallTimeout)).client; frame != nil {
		t.Fatalf("watchdog failed a prompt waiting on the user: %s", frame)
	}

	// The user answers long after the request: the adapter resumes from here.
	relay.lastActivityAt = time.Now().Add(-time.Hour)
	relayClientFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":`+frameID(t, permission)+`,"result":{"outcome":{"outcome":"selected","optionId":"allow"}}}`+"\n"), 1)
	if frame := failStalled(t, relay, 1, time.Now().Add(promptStallTimeout/2)).client; frame != nil {
		t.Fatalf("watchdog counted the wait for the user as adapter silence: %s", frame)
	}
	if frames := failStalled(t, relay, 1, time.Now().Add(2*promptStallTimeout)); frames.provider == nil {
		t.Fatal("watchdog ignored adapter silence after the user answered")
	}
}

// The adapter keeps emitting from the cancelled turn until it answers. That
// output belongs to the stalled prompt, and a late successful answer ends it
// by adoption (not by a synthetic failure), so the next prompt starts on a
// quiet adapter and the completed turn is not reported as failed.
func TestACPRelayAdoptsSuccessfulAnswerToCancelledPrompt(t *testing.T) {
	relay, promptID := startStallTestPrompt(t)
	stalledAt := time.Now().Add(2 * promptStallTimeout)
	if first := failStalled(t, relay, 1, stalledAt); len(first.provider) == 0 {
		t.Fatal("watchdog did not cancel the stalled prompt")
	}

	late, _ := relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-live","update":{"agent_message_chunk":{"content":{"type":"text","text":"tail of the old turn"}}}}}`+"\n"), 1, true)
	if !strings.Contains(string(late), ACPEventIDMetaKey) || !relay.snapshot().ActivePrompt {
		t.Fatalf("output after the cancel left the stalled prompt: active=%v frame=%s", relay.snapshot().ActivePrompt, late)
	}

	// A complete success result arriving just after the cancel must be adopted
	// as the turn result, not replaced with a stall failure frame.
	answer, _ := relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":`+promptID+`,"result":{"stopReason":"end_turn"}}`+"\n"), 1, true)
	if !strings.Contains(string(answer), `"stopReason":"end_turn"`) {
		t.Fatalf("late success answer was not adopted: %s", answer)
	}
	if strings.Contains(string(answer), ACPPromptStalledDataKey) || strings.Contains(string(answer), "provider stopped responding") {
		t.Fatalf("adopted success was replaced with a stall failure: %s", answer)
	}
	state := relay.snapshot()
	if state.ActivePrompt || state.PendingResultEventID == "" {
		t.Fatalf("post-answer state = %+v", state)
	}
	if frames := failStalled(t, relay, 1, stalledAt.Add(2*promptStallCancelGrace)); frames.client != nil || frames.provider != nil {
		t.Fatalf("watchdog acted after the adapter answered: %+v", frames)
	}
	if len(relay.abandonedPrompts) != 0 {
		t.Fatalf("an answered prompt must not be marked abandoned: %v", relay.abandonedPrompts)
	}
}

// A plain stopReason:cancelled is the adapter's acknowledgement of
// session/cancel, NOT a completed turn. It must remain a stall-marked failure
// so the controller releases the queue instead of holding it on Interrupted
// (issue #4861). This is the baseline semantics the watchdog always had.
func TestACPRelayStalledPromptEndsWhenTheAdapterAnswersTheCancel(t *testing.T) {
	relay, promptID := startStallTestPrompt(t)
	stalledAt := time.Now().Add(2 * promptStallTimeout)
	if first := failStalled(t, relay, 1, stalledAt); len(first.provider) == 0 {
		t.Fatal("watchdog did not cancel the stalled prompt")
	}

	late, _ := relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-live","update":{"agent_message_chunk":{"content":{"type":"text","text":"tail of the old turn"}}}}}`+"\n"), 1, true)
	if !strings.Contains(string(late), ACPEventIDMetaKey) || !relay.snapshot().ActivePrompt {
		t.Fatalf("output after the cancel left the stalled prompt: active=%v frame=%s", relay.snapshot().ActivePrompt, late)
	}

	answer, _ := relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":`+promptID+`,"result":{"stopReason":"cancelled"}}`+"\n"), 1, true)
	if !strings.Contains(string(answer), `"`+ACPPromptStalledDataKey+`":true`) || strings.Contains(string(answer), `"stopReason":"cancelled"`) {
		t.Fatalf("cancelled answer = %s, want the stall failure", answer)
	}
	state := relay.snapshot()
	if state.ActivePrompt || state.PendingResultEventID == "" {
		t.Fatalf("post-answer state = %+v", state)
	}
	if frames := failStalled(t, relay, 1, stalledAt.Add(2*promptStallCancelGrace)); frames.client != nil || frames.provider != nil {
		t.Fatalf("watchdog acted after the adapter answered: %+v", frames)
	}
	if len(relay.abandonedPrompts) != 0 {
		t.Fatalf("an answered prompt must not be marked abandoned: %v", relay.abandonedPrompts)
	}
}

// A late error frame for a cancelled prompt is still a failure: the relay
// forwards the stall-marked error so the controller recognizes the failed turn.
func TestACPRelayFailsLateErrorAnswerToCancelledPrompt(t *testing.T) {
	relay, promptID := startStallTestPrompt(t)
	stalledAt := time.Now().Add(2 * promptStallTimeout)
	if first := failStalled(t, relay, 1, stalledAt); len(first.provider) == 0 {
		t.Fatal("watchdog did not cancel the stalled prompt")
	}
	answer, _ := relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":`+promptID+`,"error":{"code":-32000,"message":"turn aborted"}}`+"\n"), 1, true)
	if !strings.Contains(string(answer), `"`+ACPPromptStalledDataKey+`":true`) {
		t.Fatalf("late error answer = %s, want stall-marked failure", answer)
	}
	if relay.snapshot().ActivePrompt {
		// sanity: the turn is terminal
		t.Fatal("prompt stayed active after a late error answer")
	}
}

// A success answer to a prompt that was NOT cancelled is adopted normally and
// never carries the stall marker.
func TestACPRelayAdoptsAnswerWithoutCancellation(t *testing.T) {
	relay, promptID := startStallTestPrompt(t)
	answer, _ := relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":`+promptID+`,"result":{"stopReason":"end_turn"}}`+"\n"), 1, true)
	if !strings.Contains(string(answer), `"stopReason":"end_turn"`) || strings.Contains(string(answer), ACPPromptStalledDataKey) {
		t.Fatalf("normal answer = %s", answer)
	}
}

func startStallTestPrompt(t *testing.T) (*acpRelay, string) {
	t.Helper()
	relay := newTestACPRelay(t)
	providerInit := relayClientFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"), 1)
	relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":`+frameID(t, providerInit)+`,"result":{"protocolVersion":1}}`+"\n"), 1, true)
	providerSession := relayClientFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp","mcpServers":[]}}`+"\n"), 1)
	relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":`+frameID(t, providerSession)+`,"result":{"sessionId":"session-live"}}`+"\n"), 1, true)
	providerPrompt := relayClientFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"session-live","prompt":[]}}`+"\n"), 1)
	return relay, frameID(t, providerPrompt)
}

func failStalled(t *testing.T, relay *acpRelay, generation uint64, now time.Time) acpClientFrames {
	t.Helper()
	frames, err := relay.failStalledPrompt(context.Background(), generation, true, now)
	if err != nil {
		t.Fatal(err)
	}
	return frames
}

// --- B) in-flight tool-call protection -----------------------------------

// A long-running tool call (build/test command) emits tool_call then goes
// silent. The watchdog must not cancel it at the base deadline, only at the
// longer tool-call cap. Ending the call with tool_call_update restores the
// normal deadline.
func TestACPRelayToolCallExtendsStallDeadline(t *testing.T) {
	relay, _ := startStallTestPrompt(t)
	relay.lastActivityAt = time.Now()

	relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-live","update":{"sessionUpdate":"tool_call","toolCallId":"build-1","title":"go test","kind":"execute"}}}`+"\n"), 1, true)
	if _, ok := relay.activeToolCalls["build-1"]; !ok {
		t.Fatal("tool_call did not register an active tool call")
	}

	// Base deadline passes, but the tool call is still active: no cancel.
	baseDeadline := time.Now().Add(relay.promptStallTimeout + time.Second)
	if frames := failStalled(t, relay, 1, baseDeadline); frames.provider != nil || frames.client != nil {
		t.Fatalf("watchdog killed a live tool call at the base deadline: %+v", frames)
	}

	// The adapter reports completion; the tool call leaves the active set.
	relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-live","update":{"sessionUpdate":"tool_call_update","toolCallId":"build-1","status":"completed"}}}`+"\n"), 1, true)
	if len(relay.activeToolCalls) != 0 {
		t.Fatalf("completed tool call still active: %v", relay.activeToolCalls)
	}
	if frames := failStalled(t, relay, 1, baseDeadline); frames.provider == nil {
		t.Fatal("watchdog ignored silence after the tool call finished")
	}
}

// The tool call has a hard cap so a truly dead adapter is still caught.
func TestACPRelayToolCallHasHardCap(t *testing.T) {
	relay, _ := startStallTestPrompt(t)
	relay.lastActivityAt = time.Now()
	relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"session-live","update":{"sessionUpdate":"tool_call","toolCallId":"build-1","title":"go test","kind":"execute"}}}`+"\n"), 1, true)

	if frames := failStalled(t, relay, 1, time.Now().Add(relay.toolCallTimeout()+time.Second)); frames.provider == nil {
		t.Fatal("watchdog never fired after the tool-call hard cap")
	}
}

// A tool_call_update in_progress re-asserts the call; failed/cancelled ends it.
func TestACPRelayToolCallUpdateTransitions(t *testing.T) {
	relay := newTestACPRelay(t)
	update := func(sessionUpdate, id, status string) {
		frame := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"` + sessionUpdate + `","toolCallId":"` + id + `","status":"` + status + `"}}}` + "\n"
		relayProviderFrame(t, relay, []byte(frame), 1, true)
	}
	update("tool_call", "t1", "in_progress")
	update("tool_call_update", "t2", "in_progress")
	if len(relay.activeToolCalls) != 2 {
		t.Fatalf("want two active tool calls, got %v", relay.activeToolCalls)
	}
	update("tool_call_update", "t2", "failed")
	if _, ok := relay.activeToolCalls["t2"]; ok {
		t.Fatal("failed tool call still active")
	}
	update("tool_call_update", "t1", "cancelled")
	if len(relay.activeToolCalls) != 0 {
		t.Fatalf("all tool calls should be done: %v", relay.activeToolCalls)
	}
}

// P2: a tool_call announced in a terminal state is never counted as active,
// so a provider that emits a finished call cannot dilute the watchdog budget.
func TestACPRelayTerminalToolCallNotRegistered(t *testing.T) {
	for _, status := range []string{"completed", "failed", "cancelled", "error"} {
		t.Run(status, func(t *testing.T) {
			relay := newTestACPRelay(t)
			frame := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"done","status":"` + status + `"}}}` + "\n"
			relayProviderFrame(t, relay, []byte(frame), 1, true)
			if len(relay.activeToolCalls) != 0 {
				t.Fatalf("terminal tool_call (%s) registered as active: %v", status, relay.activeToolCalls)
			}
		})
	}
}

// P3: an empty status update must not resurrect a call that already finished.
func TestACPRelayEmptyStatusDoesNotResurrectToolCall(t *testing.T) {
	relay := newTestACPRelay(t)
	frame := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call","toolCallId":"t1","status":"in_progress"}}}` + "\n"
	relayProviderFrame(t, relay, []byte(frame), 1, true)
	if _, ok := relay.activeToolCalls["t1"]; !ok {
		t.Fatal("tool_call did not register")
	}
	done := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":"completed"}}}` + "\n"
	relayProviderFrame(t, relay, []byte(done), 1, true)
	if len(relay.activeToolCalls) != 0 {
		t.Fatal("tool call did not finish")
	}
	empty := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"s","update":{"sessionUpdate":"tool_call_update","toolCallId":"t1","status":""}}}` + "\n"
	relayProviderFrame(t, relay, []byte(empty), 1, true)
	if len(relay.activeToolCalls) != 0 {
		t.Fatalf("empty status resurrected a finished tool call: %v", relay.activeToolCalls)
	}
}

// P3: tool-call events from a different session cannot dilute this turn's
// watchdog budget.
func TestACPRelayIgnoresToolCallFromOtherSession(t *testing.T) {
	relay, _ := startStallTestPrompt(t) // active session = session-live
	frame := `{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"other-session","update":{"sessionUpdate":"tool_call","toolCallId":"t1","status":"in_progress"}}}` + "\n"
	relayProviderFrame(t, relay, []byte(frame), 1, true)
	if len(relay.activeToolCalls) != 0 {
		t.Fatalf("tool call from another session was counted: %v", relay.activeToolCalls)
	}
}

// --- C) configurable threshold + logs ------------------------------------

func TestLoadPromptStallTimeout(t *testing.T) {
	cases := []struct {
		name  string
		value string
		want  time.Duration
	}{
		{"unset", "", promptStallTimeout},
		{"positive", "42s", 42 * time.Second},
		{"compound", "1h30m", 90 * time.Minute},
		{"zero fallback", "0s", promptStallTimeout},
		{"negative fallback", "-1m", promptStallTimeout},
		{"garbage fallback", "soon", promptStallTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(ACPPromptStallTimeoutEnv, tc.value)
			if got := loadPromptStallTimeout(); got != tc.want {
				t.Fatalf("loadPromptStallTimeout() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestACPRelayUsesConfiguredStallTimeout(t *testing.T) {
	t.Setenv(ACPPromptStallTimeoutEnv, "1h")
	relay := newTestACPRelay(t)
	if relay.promptStallTimeout != time.Hour {
		t.Fatalf("relay timeout = %v, want 1h", relay.promptStallTimeout)
	}
	relay, promptID := startStallTestPromptWithRelay(t, relay)

	// Just past the configured deadline the watchdog cancels.
	if frames := failStalled(t, relay, 1, time.Now().Add(time.Hour+time.Second)); frames.provider == nil {
		t.Fatal("configured timeout was not applied to the watchdog")
	}
	// Adopt the late success so the prompt is terminal.
	relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":`+promptID+`,"result":{"stopReason":"end_turn"}}`+"\n"), 1, true)
}

func startStallTestPromptWithRelay(t *testing.T, relay *acpRelay) (*acpRelay, string) {
	t.Helper()
	providerInit := relayClientFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`+"\n"), 1)
	relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":`+frameID(t, providerInit)+`,"result":{"protocolVersion":1}}`+"\n"), 1, true)
	providerSession := relayClientFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/tmp","mcpServers":[]}}`+"\n"), 1)
	relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":`+frameID(t, providerSession)+`,"result":{"sessionId":"session-live"}}`+"\n"), 1, true)
	providerPrompt := relayClientFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"session-live","prompt":[]}}`+"\n"), 1)
	return relay, frameID(t, providerPrompt)
}

func TestACPRelayStallLogsStructuredEvents(t *testing.T) {
	var buf bytes.Buffer
	relay := newTestACPRelay(t)
	relay.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	relay, promptID := startStallTestPromptWithRelay(t, relay)

	stalledAt := time.Now().Add(2 * relay.promptStallTimeout)
	failStalled(t, relay, 1, stalledAt)                                                                                          // stall_cancel_sent
	relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":`+promptID+`,"result":{"stopReason":"end_turn"}}`+"\n"), 1, true) // late_result_adopted

	logs := buf.String()
	for _, want := range []string{"stall_cancel_sent", "late_result_adopted", "session_id", "prompt_id", "silent_for"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("structured log missing %q: %s", want, logs)
		}
	}
}

func TestACPRelayStallLogsDiscardAfterFailure(t *testing.T) {
	var buf bytes.Buffer
	relay := newTestACPRelay(t)
	relay.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo}))
	relay, promptID := startStallTestPromptWithRelay(t, relay)

	stalledAt := time.Now().Add(2 * relay.promptStallTimeout)
	failStalled(t, relay, 1, stalledAt)
	failStalled(t, relay, 1, stalledAt.Add(promptStallCancelGrace))                                                              // stall_failure_emitted
	relayProviderFrame(t, relay, []byte(`{"jsonrpc":"2.0","id":`+promptID+`,"result":{"stopReason":"end_turn"}}`+"\n"), 1, true) // late_result_discarded

	logs := buf.String()
	for _, want := range []string{"stall_failure_emitted", "late_result_discarded", "silent_for"} {
		if !strings.Contains(logs, want) {
			t.Fatalf("structured log missing %q: %s", want, logs)
		}
	}
}
